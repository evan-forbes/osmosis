package concentrated_liquidity

import (
	"errors"
	"fmt"
	"strconv"
	"time"

	sdk "github.com/cosmos/cosmos-sdk/types"

	"github.com/osmosis-labs/osmosis/v15/x/concentrated-liquidity/internal/math"
	types "github.com/osmosis-labs/osmosis/v15/x/concentrated-liquidity/types"
)

// createPosition creates a concentrated liquidity position in range between lowerTick and upperTick
// in a given `PoolId with the desired amount of each token. Since LPs are only allowed to provide
// liquidity proportional to the existing reserves, the actual amount of tokens used might differ from requested.
// As a result, LPs may also provide the minimum amount of each token to be used so that the system fails
// to create position if the desired amounts cannot be satisfied.
// On success, returns an actual amount of each token used and liquidity created.
// Returns error if:
// - the provided ticks are out of range / invalid
// - the pool provided does not exist
// - the liquidity delta is zero
// - the amount0 or amount1 returned from the position update is less than the given minimums
// - the pool or user does not have enough tokens to satisfy the requested amount
func (k Keeper) createPosition(ctx sdk.Context, poolId uint64, owner sdk.AccAddress, amount0Desired, amount1Desired, amount0Min, amount1Min sdk.Int, lowerTick, upperTick int64) (uint64, sdk.Int, sdk.Int, sdk.Dec, time.Time, error) {
	// get current blockTime that user joins the position
	joinTime := ctx.BlockTime()

	// Retrieve the pool associated with the given pool ID.
	pool, err := k.getPoolById(ctx, poolId)
	if err != nil {
		return 0, sdk.Int{}, sdk.Int{}, sdk.Dec{}, time.Time{}, err
	}
	// Check if the provided tick range is valid according to the pool's tick spacing and module parameters.
	if err := validateTickRangeIsValid(pool.GetTickSpacing(), pool.GetExponentAtPriceOne(), lowerTick, upperTick); err != nil {
		return 0, sdk.Int{}, sdk.Int{}, sdk.Dec{}, time.Time{}, err
	}

	// Transform the provided ticks into their corresponding sqrtPrices.
	sqrtPriceLowerTick, sqrtPriceUpperTick, err := math.TicksToSqrtPrice(lowerTick, upperTick, pool.GetExponentAtPriceOne())
	if err != nil {
		return 0, sdk.Int{}, sdk.Int{}, sdk.Dec{}, time.Time{}, err
	}

	// Create a cache context for the current transaction.
	// This allows us to make changes to the context without persisting it until later.
	// We only write the cache context (i.e. persist the changes) if the actual amounts returned
	// are greater than the given minimum amounts.
	cacheCtx, writeCacheCtx := ctx.CacheContext()
	initialSqrtPrice := pool.GetCurrentSqrtPrice()
	initialTick := pool.GetCurrentTick()
	positionId := k.getNextPositionIdAndIncrement(ctx)

	// If the current square root price and current tick are zero, then this is the first position to be created for this pool.
	// In this case, we calculate the square root price and current tick based on the inputs of this position.
	if k.isInitialPositionForPool(initialSqrtPrice, initialTick) {
		err := k.initializeInitialPositionForPool(cacheCtx, pool, amount0Desired, amount1Desired)
		if err != nil {
			return 0, sdk.Int{}, sdk.Int{}, sdk.Dec{}, time.Time{}, err
		}
	}

	// Calculate the amount of liquidity that will be added to the pool by creating this position.
	liquidityDelta := math.GetLiquidityFromAmounts(pool.GetCurrentSqrtPrice(), sqrtPriceLowerTick, sqrtPriceUpperTick, amount0Desired, amount1Desired)
	if liquidityDelta.IsZero() {
		return 0, sdk.Int{}, sdk.Int{}, sdk.Dec{}, time.Time{}, errors.New("liquidityDelta calculated equals zero")
	}

	if err := k.initializeFeeAccumulatorPosition(cacheCtx, poolId, lowerTick, upperTick, positionId); err != nil {
		return 0, sdk.Int{}, sdk.Int{}, sdk.Dec{}, time.Time{}, err
	}

	// Update the position in the pool based on the provided tick range and liquidity delta.
	actualAmount0, actualAmount1, err := k.updatePosition(cacheCtx, poolId, owner, lowerTick, upperTick, liquidityDelta, joinTime, positionId)
	if err != nil {
		return 0, sdk.Int{}, sdk.Int{}, sdk.Dec{}, time.Time{}, err
	}

	// Check if the actual amounts of tokens 0 and 1 are greater than or equal to the given minimum amounts.
	if actualAmount0.LT(amount0Min) {
		return 0, sdk.Int{}, sdk.Int{}, sdk.Dec{}, time.Time{}, types.InsufficientLiquidityCreatedError{Actual: actualAmount0, Minimum: amount0Min, IsTokenZero: true}
	}
	if actualAmount1.LT(amount1Min) {
		return 0, sdk.Int{}, sdk.Int{}, sdk.Dec{}, time.Time{}, types.InsufficientLiquidityCreatedError{Actual: actualAmount1, Minimum: amount1Min}
	}

	// Transfer the actual amounts of tokens 0 and 1 from the position owner to the pool.
	err = k.sendCoinsBetweenPoolAndUser(cacheCtx, pool.GetToken0(), pool.GetToken1(), actualAmount0, actualAmount1, owner, pool.GetAddress())
	if err != nil {
		return 0, sdk.Int{}, sdk.Int{}, sdk.Dec{}, time.Time{}, err
	}

	// Persist the changes made to the cache context if the actual amounts of tokens 0 and 1 are greater than or equal to the given minimum amounts.
	writeCacheCtx()

	emitLiquidityChangeEvent(ctx, types.TypeEvtCreatePosition, positionId, owner, poolId, lowerTick, upperTick, joinTime, liquidityDelta, actualAmount0, actualAmount1)

	return positionId, actualAmount0, actualAmount1, liquidityDelta, joinTime, nil
}

// withdrawPosition attempts to withdraw liquidityAmount from a position with the given pool id in the given tick range.
// On success, returns a positive amount of each token withdrawn.
// Returns error if
// - there is no position in the given tick ranges
// - if tick ranges are invalid
// - if attempts to withdraw an amount higher than originally provided in createPosition for a given range.
func (k Keeper) withdrawPosition(ctx sdk.Context, owner sdk.AccAddress, positionId uint64, requestedLiquidityAmountToWithdraw sdk.Dec) (amtDenom0, amtDenom1 sdk.Int, err error) {
	position, err := k.GetPosition(ctx, positionId)
	if err != nil {
		return sdk.Int{}, sdk.Int{}, err
	}

	// Retrieve the pool associated with the given pool ID.
	pool, err := k.getPoolById(ctx, position.PoolId)
	if err != nil {
		return sdk.Int{}, sdk.Int{}, err
	}

	// Check if the provided tick range is valid according to the pool's tick spacing and module parameters.
	if err := validateTickRangeIsValid(pool.GetTickSpacing(), pool.GetExponentAtPriceOne(), position.LowerTick, position.UpperTick); err != nil {
		return sdk.Int{}, sdk.Int{}, err
	}

	// Retrieve the position in the pool for the provided owner and tick range.
	availableLiquidity, err := k.GetPositionLiquidity(ctx, positionId)
	if err != nil {
		return sdk.Int{}, sdk.Int{}, err
	}

	_, err = k.collectIncentives(ctx, owner, positionId)
	if err != nil {
		return sdk.Int{}, sdk.Int{}, err
	}

	// Check if the requested liquidity amount to withdraw is less than or equal to the available liquidity for the position.
	// If it is greater than the available liquidity, return an error.
	if requestedLiquidityAmountToWithdraw.GT(availableLiquidity) {
		return sdk.Int{}, sdk.Int{}, types.InsufficientLiquidityError{Actual: requestedLiquidityAmountToWithdraw, Available: availableLiquidity}
	}

	// Calculate the change in liquidity for the pool based on the requested amount to withdraw.
	// This amount is negative because that liquidity is being withdrawn from the pool.
	liquidityDelta := requestedLiquidityAmountToWithdraw.Neg()

	// Update the position in the pool based on the provided tick range and liquidity delta.
	actualAmount0, actualAmount1, err := k.updatePosition(ctx, position.PoolId, owner, position.LowerTick, position.UpperTick, liquidityDelta, position.JoinTime, positionId)
	if err != nil {
		return sdk.Int{}, sdk.Int{}, err
	}

	// Transfer the actual amounts of tokens 0 and 1 from the pool to the position owner.
	err = k.sendCoinsBetweenPoolAndUser(ctx, pool.GetToken0(), pool.GetToken1(), actualAmount0.Abs(), actualAmount1.Abs(), pool.GetAddress(), owner)
	if err != nil {
		return sdk.Int{}, sdk.Int{}, err
	}

	// If the requested liquidity amount to withdraw is equal to the available liquidity, delete the position from state.
	// Ensure we collect any outstanding fees and incentives prior to deleting the position from state. This claiming
	// process also clears position records from fee and incentive accumulators.
	if requestedLiquidityAmountToWithdraw.Equal(availableLiquidity) {
		if _, err := k.collectFees(ctx, owner, positionId); err != nil {
			return sdk.Int{}, sdk.Int{}, err
		}

		if _, err := k.collectIncentives(ctx, owner, positionId); err != nil {
			return sdk.Int{}, sdk.Int{}, err
		}

		if err := k.deletePosition(ctx, positionId, owner, position.PoolId); err != nil {
			return sdk.Int{}, sdk.Int{}, err
		}
	}

	emitLiquidityChangeEvent(ctx, types.TypeEvtWithdrawPosition, positionId, owner, position.PoolId, position.LowerTick, position.UpperTick, position.JoinTime, liquidityDelta, actualAmount0, actualAmount1)

	return actualAmount0.Neg(), actualAmount1.Neg(), nil
}

// updatePosition updates the position in the given pool id and in the given tick range and liquidityAmount.
// Negative liquidityDelta implies withdrawing liquidity.
// Positive liquidityDelta implies adding liquidity.
// Updates ticks and pool liquidity. Returns how much of each token is either added or removed.
// Negative returned amounts imply that tokens are removed from the pool.
// Positive returned amounts imply that tokens are added to the pool.
func (k Keeper) updatePosition(ctx sdk.Context, poolId uint64, owner sdk.AccAddress, lowerTick, upperTick int64, liquidityDelta sdk.Dec, joinTime time.Time, positionId uint64) (sdk.Int, sdk.Int, error) {
	// now calculate amount for token0 and token1
	pool, err := k.getPoolById(ctx, poolId)
	if err != nil {
		return sdk.Int{}, sdk.Int{}, err
	}

	currentTick := pool.GetCurrentTick().Int64()

	// update tickInfo state
	// TODO: come back to sdk.Int vs sdk.Dec state & truncation
	err = k.initOrUpdateTick(ctx, poolId, currentTick, lowerTick, liquidityDelta, false)
	if err != nil {
		return sdk.Int{}, sdk.Int{}, err
	}

	// TODO: come back to sdk.Int vs sdk.Dec state & truncation
	err = k.initOrUpdateTick(ctx, poolId, currentTick, upperTick, liquidityDelta, true)
	if err != nil {
		return sdk.Int{}, sdk.Int{}, err
	}

	// update position state
	// TODO: come back to sdk.Int vs sdk.Dec state & truncation
	err = k.initOrUpdatePosition(ctx, poolId, owner, lowerTick, upperTick, liquidityDelta, joinTime, positionId)
	if err != nil {
		return sdk.Int{}, sdk.Int{}, err
	}

	// Transform the provided ticks into their corresponding sqrtPrices.
	sqrtPriceLowerTick, sqrtPriceUpperTick, err := math.TicksToSqrtPrice(lowerTick, upperTick, pool.GetExponentAtPriceOne())
	if err != nil {
		return sdk.Int{}, sdk.Int{}, err
	}

	actualAmount0, actualAmount1 := pool.CalcActualAmounts(ctx, lowerTick, upperTick, sqrtPriceLowerTick, sqrtPriceUpperTick, liquidityDelta)
	if err != nil {
		return sdk.Int{}, sdk.Int{}, err
	}

	pool.UpdateLiquidityIfActivePosition(ctx, lowerTick, upperTick, liquidityDelta)

	if err := k.setPool(ctx, pool); err != nil {
		return sdk.Int{}, sdk.Int{}, err
	}

	// TODO: test https://github.com/osmosis-labs/osmosis/issues/3997
	if err := k.updateFeeAccumulatorPosition(ctx, liquidityDelta, positionId); err != nil {
		return sdk.Int{}, sdk.Int{}, err
	}

	// The returned amounts are rounded down to avoid returning more to clients than they actually deposited.
	return actualAmount0.TruncateInt(), actualAmount1.TruncateInt(), nil
}

// sendCoinsBetweenPoolAndUser takes the amounts calculated from a join/exit position and executes the send between pool and user
func (k Keeper) sendCoinsBetweenPoolAndUser(ctx sdk.Context, denom0, denom1 string, amount0, amount1 sdk.Int, sender, receiver sdk.AccAddress) error {
	if amount0.IsNegative() {
		return fmt.Errorf("amount0 is negative: %s", amount0)
	}
	if amount1.IsNegative() {
		return fmt.Errorf("amount1 is negative: %s", amount1)
	}

	finalCoinsToSend := sdk.NewCoins(sdk.NewCoin(denom1, amount1), sdk.NewCoin(denom0, amount0))

	err := k.bankKeeper.SendCoins(ctx, sender, receiver, finalCoinsToSend)
	if err != nil {
		return err
	}
	return nil
}

// isInitialPositionForPool checks if the initial sqrtPrice and initial tick are equal to zero.
// If so, this is the first position to be created for this pool, and we return true.
// If not, we return false.
func (k Keeper) isInitialPositionForPool(initialSqrtPrice sdk.Dec, initialTick sdk.Int) bool {
	if initialSqrtPrice.Equal(sdk.ZeroDec()) && initialTick.Equal(sdk.ZeroInt()) {
		return true
	}
	return false
}

// createInitialPosition ensures that the first position created on this pool includes both asset0 and asset1
// This is required so we can set the pool's sqrtPrice and calculate it's initial tick from this
func (k Keeper) initializeInitialPositionForPool(ctx sdk.Context, pool types.ConcentratedPoolExtension, amount0Desired, amount1Desired sdk.Int) error {
	// Check that the position includes some amount of both asset0 and asset1
	if !amount0Desired.GT(sdk.ZeroInt()) || !amount1Desired.GT(sdk.ZeroInt()) {
		return types.InitialLiquidityZeroError{Amount0: amount0Desired, Amount1: amount1Desired}
	}

	// Calculate the spot price and sqrt price from the amount provided
	initialSpotPrice := amount1Desired.ToDec().Quo(amount0Desired.ToDec())
	initialSqrtPrice, err := initialSpotPrice.ApproxSqrt()
	if err != nil {
		return err
	}

	// Calculate the initial tick from the initial spot price
	initialTick, err := math.PriceToTick(initialSpotPrice, pool.GetExponentAtPriceOne())
	if err != nil {
		return err
	}

	// Set the pool's current sqrt price and current tick to the above calculated values
	pool.SetCurrentSqrtPrice(initialSqrtPrice)
	pool.SetCurrentTick(initialTick)
	err = k.setPool(ctx, pool)
	if err != nil {
		return err
	}
	return nil
}

// emitLiquidityChangeEvent emits an event for a liquidity change when creating or withdrawing a position.
// It emits all of the fields uniquely identifying a position such as:
// - position id
// - sender
// - pool id
// - join time
// - lower tick
// - upper tick
// It also emits additional attributes for the liquidity added or removed and the actual amounts of asset0 and asset1 it translates to.
func emitLiquidityChangeEvent(ctx sdk.Context, eventType string, positionId uint64, sender sdk.AccAddress, poolId uint64, lowerTick, upperTick int64, joinTime time.Time, liquidityDelta sdk.Dec, actualAmount0, actualAmount1 sdk.Int) {
	ctx.EventManager().EmitEvent(sdk.NewEvent(
		eventType,
		sdk.NewAttribute(sdk.AttributeKeyModule, types.AttributeValueCategory),
		sdk.NewAttribute(types.AttributeKeyPositionId, strconv.FormatUint(positionId, 10)),
		sdk.NewAttribute(sdk.AttributeKeySender, sender.String()),
		sdk.NewAttribute(types.AttributeKeyPoolId, strconv.FormatUint(poolId, 10)),
		sdk.NewAttribute(types.AttributeLowerTick, strconv.FormatInt(lowerTick, 10)),
		sdk.NewAttribute(types.AttributeUpperTick, strconv.FormatInt(upperTick, 10)),
		sdk.NewAttribute(types.AttributeJoinTime, joinTime.String()),
		sdk.NewAttribute(types.AttributeLiquidity, liquidityDelta.String()),
		sdk.NewAttribute(types.AttributeAmount0, actualAmount0.String()),
		sdk.NewAttribute(types.AttributeAmount1, actualAmount1.String()),
	))
}
