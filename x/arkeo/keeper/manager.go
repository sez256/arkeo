package keeper

import (
	"arkeo/common"
	"arkeo/common/cosmos"
	"arkeo/x/arkeo/configs"
	"arkeo/x/arkeo/types"

	sdkerrors "github.com/cosmos/cosmos-sdk/types/errors"
	stakingtypes "github.com/cosmos/cosmos-sdk/x/staking/types"
)

type Manager struct {
	keeper  Keeper
	configs configs.ConfigValues
}

func NewManager(k Keeper) Manager {
	ver := k.GetVersion()
	return Manager{
		keeper:  k,
		configs: configs.GetConfigValues(ver),
	}
}

func (mgr Manager) EndBlock(ctx cosmos.Context) error {
	if err := mgr.ContractEndBlock(ctx); err != nil {
		ctx.Logger().Error("unable to settle contracts", "error", err)
	}
	if err := mgr.ValidatorEndBlock(ctx); err != nil {
		ctx.Logger().Error("unable to settle contracts", "error", err)
	}
	return nil
}

func (mgr Manager) ContractEndBlock(ctx cosmos.Context) error {
	set, err := mgr.keeper.GetContractExpirationSet(ctx, ctx.BlockHeight())
	if err != nil {
		return err
	}

	for _, exp := range set.Contracts {
		contract, err := mgr.keeper.GetContract(ctx, exp.ProviderPubKey, exp.Chain, exp.Client)
		if err != nil {
			ctx.Logger().Error("unable to fetch contract", "pubkey", exp.ProviderPubKey, "chain", exp.Chain, "client", exp.Client, "error", err)
			continue
		}
		_, err = mgr.SettleContract(ctx, contract, 0, true)
		if err != nil {
			ctx.Logger().Error("unable settle contract", "pubkey", exp.ProviderPubKey, "chain", exp.Chain, "client", exp.Client, "error", err)
			continue
		}
	}

	return nil
}

// This function pays out rewards to validators.
// TODO: the method of accomplishing this is admittedly quite inefficient. The
// better approach would be to track live allocation via assigning "units" to
// validators when they bond and unbond. The math for this is as follows
// U = total bond units
// T = tokens bonded
// t = new tokens being bonded
// units = U / (T / t)
// Since the development goal at the moment is to get this chain up and
// running, we can save this optimization for another day.
func (mgr Manager) ValidatorEndBlock(ctx cosmos.Context) error {
	valCycle := mgr.FetchConfig(ctx, configs.ValidatorPayoutCycle)
	if valCycle == 0 || ctx.BlockHeight()%valCycle != 0 {
		return nil
	}
	validators := mgr.keeper.GetActiveValidators(ctx)

	reserve := mgr.keeper.GetBalanceOfModule(ctx, types.ReserveName, configs.Denom)
	emissionCurve := mgr.FetchConfig(ctx, configs.EmissionCurve)
	blocksPerYear := mgr.FetchConfig(ctx, configs.BlocksPerYear)
	blockReward := mgr.calcBlockReward(reserve.Int64(), emissionCurve, (blocksPerYear / valCycle))

	if blockReward.IsZero() {
		ctx.Logger().Info("no validator rewards this block")
		return nil
	}

	// sum tokens
	total := cosmos.ZeroInt()
	for _, val := range validators {
		if val.Status != stakingtypes.Bonded {
			continue
		}
		total = total.Add(val.Tokens)
	}

	// TODO: pay delegates of validators, maybe using vesting accounts?

	for _, val := range validators {
		if val.Status != stakingtypes.Bonded {
			continue
		}
		acc, err := cosmos.AccAddressFromBech32(val.OperatorAddress)
		if err != nil {
			ctx.Logger().Error("unable to parse validator operator address", "error", err)
			continue
		}

		rwd := common.GetSafeShare(val.Tokens, total, blockReward)
		rewards := getCoins(rwd.Int64())

		if err := mgr.keeper.SendFromModuleToAccount(ctx, types.ReserveName, acc, rewards); err != nil {
			ctx.Logger().Error("unable to pay rewards to validator", "validator", val.OperatorAddress, "error", err)
			continue
		}
		ctx.Logger().Info("validator rewarded", "validator", acc.String(), "amount", rwd)

		mgr.ValidatorPayoutEvent(ctx, acc, rwd)
	}

	return nil
}

func (mgr Manager) calcBlockReward(totalReserve, emissionCurve, blocksPerYear int64) cosmos.Int {
	// Block Rewards will take the latest reserve, divide it by the emission
	// curve factor, then divide by blocks per year
	if emissionCurve == 0 || blocksPerYear == 0 {
		return cosmos.ZeroInt()
	}
	trD := cosmos.NewDec(totalReserve)
	ecD := cosmos.NewDec(emissionCurve)
	bpyD := cosmos.NewDec(blocksPerYear)
	return trD.Quo(ecD).Quo(bpyD).RoundInt()
}

func (mgr Manager) FetchConfig(ctx cosmos.Context, name configs.ConfigName) int64 {
	// TODO: create a handler for admins to be able to change configs on the
	// fly and check them here before returning
	return mgr.configs.GetInt64Value(name)
}

// any owed debt is paid to data provider
func (mgr Manager) SettleContract(ctx cosmos.Context, contract types.Contract, nonce int64, closed bool) (types.Contract, error) {
	if nonce > contract.Nonce {
		contract.Nonce = nonce
	}
	totalDebt, err := mgr.contractDebt(ctx, contract)
	valIncome := common.GetSafeShare(cosmos.NewInt(mgr.FetchConfig(ctx, configs.ReserveTax)), cosmos.NewInt(configs.MaxBasisPoints), totalDebt)
	debt := totalDebt.Sub(valIncome)
	if err != nil {
		return contract, err
	}
	if !debt.IsZero() {
		provider, err := contract.ProviderPubKey.GetMyAddress()
		if err != nil {
			return contract, err
		}
		if err := mgr.keeper.SendFromModuleToAccount(ctx, types.ContractName, provider, cosmos.NewCoins(cosmos.NewCoin(configs.Denom, debt))); err != nil {
			return contract, err
		}
		if err := mgr.keeper.SendFromModuleToModule(ctx, types.ContractName, types.ReserveName, cosmos.NewCoins(cosmos.NewCoin(configs.Denom, valIncome))); err != nil {
			return contract, err
		}
	}

	contract.Paid = contract.Paid.Add(totalDebt)
	if closed {
		remainder := contract.Deposit.Sub(contract.Paid)
		if !remainder.IsZero() {
			client, err := contract.Client.GetMyAddress()
			if err != nil {
				return contract, err
			}
			if err := mgr.keeper.SendFromModuleToAccount(ctx, types.ContractName, client, cosmos.NewCoins(cosmos.NewCoin(configs.Denom, remainder))); err != nil {
				return contract, err
			}
		}
		contract.ClosedHeight = ctx.BlockHeight()
	}

	err = mgr.keeper.SetContract(ctx, contract)
	if err != nil {
		return contract, err
	}

	mgr.ContractSettlementEvent(ctx, debt, contract)
	return contract, nil
}

func (mgr Manager) contractDebt(ctx cosmos.Context, contract types.Contract) (cosmos.Int, error) {
	var debt cosmos.Int
	switch contract.Type {
	case types.ContractType_Subscription:
		debt = cosmos.NewInt(contract.Rate * (ctx.BlockHeight() - contract.Height)).Sub(contract.Paid)
	case types.ContractType_PayAsYouGo:
		debt = cosmos.NewInt(contract.Rate * contract.Nonce).Sub(contract.Paid)
	default:
		return cosmos.ZeroInt(), sdkerrors.Wrapf(types.ErrInvalidContractType, "%s", contract.Type.String())
	}

	if debt.IsNegative() {
		return cosmos.ZeroInt(), nil
	}

	// sanity check, ensure provider cannot take more than deposited into the contract
	if contract.Paid.Add(debt).GT(contract.Deposit) {
		return contract.Deposit.Sub(contract.Paid), nil
	}

	return debt, nil
}