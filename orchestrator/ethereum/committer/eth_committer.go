package committer

import (
	"context"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	ethcmn "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/pkg/errors"
	"github.com/rs/zerolog"
	"github.com/umee-network/peggo/orchestrator/ethereum/provider"
	"github.com/umee-network/peggo/orchestrator/ethereum/util"
)

// NewEthCommitter returns an instance of EVMCommitter, which
// can be used to submit txns into Ethereum, Matic, and other EVM-compatible networks.
func NewEthCommitter(
	logger zerolog.Logger,
	fromAddress ethcmn.Address,
	ethGasPriceAdjustment float64,
	ethGasLimitAdjustment float64,
	fromSigner bind.SignerFn,
	evmProvider provider.EVMProviderWithRet,
	committerOpts ...EVMCommitterOption,
) (EVMCommitter, error) {
	committer := &ethCommitter{
		logger:                logger.With().Str("module", "ethCommiter").Logger(),
		committerOpts:         defaultOptions(),
		ethGasPriceAdjustment: ethGasPriceAdjustment,
		ethGasLimitAdjustment: ethGasLimitAdjustment,
		fromAddress:           fromAddress,
		fromSigner:            fromSigner,
		evmProvider:           evmProvider,
		nonceCache:            util.NewNonceCache(),
	}

	if err := applyOptions(committer.committerOpts, committerOpts...); err != nil {
		return nil, err
	}

	committer.nonceCache.Sync(fromAddress, func() (uint64, error) {
		nonce, err := evmProvider.PendingNonceAt(context.TODO(), fromAddress)
		return nonce, err
	})

	return committer, nil
}

type ethCommitter struct {
	logger        zerolog.Logger
	committerOpts *options

	fromAddress ethcmn.Address
	fromSigner  bind.SignerFn

	ethGasPriceAdjustment float64
	ethGasLimitAdjustment float64
	evmProvider           provider.EVMProviderWithRet
	nonceCache            util.NonceCache
}

func (e *ethCommitter) FromAddress() ethcmn.Address {
	return e.fromAddress
}

func (e *ethCommitter) Provider() provider.EVMProvider {
	return e.evmProvider
}

func (e *ethCommitter) EstimateGas(
	ctx context.Context,
	recipient ethcmn.Address,
	txData []byte,
) (gasCost uint64, gasPrice *big.Int, err error) {

	opts := &bind.TransactOpts{
		From:     e.fromAddress,
		Signer:   e.fromSigner,
		GasPrice: e.committerOpts.GasPrice.BigInt(),
		GasLimit: e.committerOpts.GasLimit,
		Context:  ctx, // with RPC timeout
	}

	suggestedGasPrice, err := e.evmProvider.SuggestGasPrice(opts.Context)
	if err != nil {
		return 0, nil, errors.Errorf("failed to suggest gas price: %v", err)
	}

	// Suggested gas price may not be accurate, so we multiply the result by the gas price adjustment factor.
	incrementedPrice := big.NewFloat(0).Mul(
		new(big.Float).SetInt(suggestedGasPrice),
		big.NewFloat(e.ethGasPriceAdjustment),
	)

	gasPrice = new(big.Int)
	incrementedPrice.Int(gasPrice)

	opts.GasPrice = gasPrice
	msg := ethereum.CallMsg{From: opts.From, To: &recipient, GasPrice: gasPrice, Value: nil, Data: txData}

	gasCost, err = e.evmProvider.EstimateGas(ctx, msg)

	// Estimated gas cost may not be accurate, so we multiply the result by the gas limit adjustment factor.
	gasCost = uint64(float64(gasCost) * e.ethGasLimitAdjustment)

	return gasCost, gasPrice, err
}

func (e *ethCommitter) SendTx(
	ctx context.Context,
	recipient ethcmn.Address,
	txData []byte,
	gasCost uint64,
	gasPrice *big.Int,
) (txHash ethcmn.Hash, err error) {
	opts := &bind.TransactOpts{
		From:   e.fromAddress,
		Signer: e.fromSigner,

		GasPrice: gasPrice,
		GasLimit: gasCost,
		Context:  ctx, // with RPC timeout
	}

	resyncNonces := func(from ethcmn.Address) {
		e.nonceCache.Sync(from, func() (uint64, error) {
			nonce, err := e.evmProvider.PendingNonceAt(context.TODO(), from)
			if err != nil {
				e.logger.Err(err).Msg("unable to acquire nonce")
			}

			return nonce, err
		})
	}

	if err := e.nonceCache.Serialize(e.fromAddress, func() (err error) {
		nonce, _ := e.nonceCache.Get(e.fromAddress)
		var resyncUsed bool

		for {
			opts.Nonce = big.NewInt(nonce)
			var cancel context.CancelFunc
			opts.Context, cancel = context.WithTimeout(ctx, e.committerOpts.RPCTimeout)
			defer cancel()

			tx := types.NewTransaction(opts.Nonce.Uint64(), recipient, nil, opts.GasLimit, opts.GasPrice, txData)
			signedTx, err := opts.Signer(opts.From, tx)
			if err != nil {
				err := errors.Wrap(err, "failed to sign transaction")
				return err
			}

			txHash = signedTx.Hash()

			txHashRet, err := e.evmProvider.SendTransactionWithRet(opts.Context, signedTx)
			if err == nil {
				// override with a real hash from node resp
				txHash = txHashRet
				e.nonceCache.Incr(e.fromAddress)
				return nil
			}

			e.logger.Err(err).
				Str("tx_hash", txHash.Hex()).
				Str("tx_hash_ret", txHashRet.Hex()).
				Msg("sendTransaction failed")

			switch {
			case strings.Contains(err.Error(), "invalid sender"):
				err := errors.New("failed to sign transaction")
				e.nonceCache.Incr(e.fromAddress)
				return err
			case strings.Contains(err.Error(), "nonce too low"),
				strings.Contains(err.Error(), "nonce too high"),
				strings.Contains(err.Error(), "the tx doesn't have the correct nonce"):

				if resyncUsed {
					e.logger.Error().
						Str("from_address", e.fromAddress.Hex()).
						Int64("nonce", nonce).
						Msg("nonces synced, but still wrong nonce for address")
					err = errors.Wrapf(err, "nonce %d mismatch", nonce)
					return err
				}

				resyncNonces(e.fromAddress)

				resyncUsed = true
				// try again with updated nonce
				nonce, _ = e.nonceCache.Get(e.fromAddress)
				opts.Nonce = big.NewInt(nonce)

				continue

			default:
				if strings.Contains(err.Error(), "known transaction") {
					// skip one nonce step, try to send again
					nonce := e.nonceCache.Incr(e.fromAddress)
					opts.Nonce = big.NewInt(nonce)
					continue
				}

				if strings.Contains(err.Error(), "VM Exception") {
					// a VM execution consumes gas and nonce is increasing
					e.nonceCache.Incr(e.fromAddress)
					return err
				}

				return err
			}
		}
	}); err != nil {
		return ethcmn.Hash{}, err
	}

	return txHash, nil
}
