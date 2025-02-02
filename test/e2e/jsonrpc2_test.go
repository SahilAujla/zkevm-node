package e2e

import (
	"context"
	"encoding/json"
	"math/big"
	"strings"
	"testing"

	"github.com/0xPolygonHermez/zkevm-node/hex"
	"github.com/0xPolygonHermez/zkevm-node/jsonrpc/client"
	"github.com/0xPolygonHermez/zkevm-node/jsonrpc/types"
	"github.com/0xPolygonHermez/zkevm-node/log"
	"github.com/0xPolygonHermez/zkevm-node/test/contracts/bin/Revert"
	"github.com/0xPolygonHermez/zkevm-node/test/contracts/bin/Revert2"
	"github.com/0xPolygonHermez/zkevm-node/test/contracts/bin/Storage"
	"github.com/0xPolygonHermez/zkevm-node/test/operations"
	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	ethTypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func Test_Misc(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}
	ctx := context.Background()
	setup()
	defer teardown()
	for _, network := range networks {
		log.Infof("Network %s", network.Name)

		ethereumClient, err := ethclient.Dial(network.URL)
		require.NoError(t, err)

		// ChainId()
		chainId, err := ethereumClient.ChainID(ctx)
		require.NoError(t, err)
		require.Equal(t, network.ChainID, chainId.Uint64())

		// Syncing()
		progress, err := ethereumClient.SyncProgress(ctx)
		require.NoError(t, err)
		if progress != nil {
			log.Info("Its syncing")
			blockNumber, err := ethereumClient.BlockNumber(ctx)
			require.NoError(t, err)
			// if it's actually syncing
			require.Equal(t, uint64(0x0), progress.StartingBlock)
			require.Equal(t, blockNumber, progress.CurrentBlock)
		}

		// GetStorageAt()

		// first deploy sample smart contract
		sc_payload := int64(42)
		sc_retrieve := common.HexToHash("0x2a")
		auth, err := operations.GetAuth(operations.DefaultSequencerPrivateKey, network.ChainID)
		require.NoError(t, err)
		contractAddress, tx, storageSC, err := Storage.DeployStorage(auth, ethereumClient)
		require.NoError(t, err)
		err = operations.WaitTxToBeMined(ctx, ethereumClient, tx, operations.DefaultTimeoutTxToBeMined)
		require.NoError(t, err)
		tx, err = storageSC.Store(auth, big.NewInt(sc_payload))
		require.NoError(t, err)
		err = operations.WaitTxToBeMined(ctx, ethereumClient, tx, operations.DefaultTimeoutTxToBeMined)
		require.NoError(t, err)

		storage, err := ethereumClient.StorageAt(ctx, contractAddress, common.HexToHash("0x0"), nil)
		require.NoError(t, err)
		// in bytes but has to be hash 0x0...42
		require.Equal(t, sc_retrieve, common.BytesToHash(storage))

		// eth_getCode

		scBytecode, err := ethereumClient.CodeAt(ctx, contractAddress, nil)
		require.NoError(t, err)
		require.Contains(t, Storage.StorageMetaData.Bin, common.Bytes2Hex(scBytecode))

		emptyBytecode, err := ethereumClient.CodeAt(ctx, common.HexToAddress("0xdeadbeef"), nil)
		require.NoError(t, err)
		require.Empty(t, emptyBytecode)

		// check for request having more params than required:

		response, err := client.JSONRPCCall(network.URL, "eth_chainId", common.HexToHash("0x70997970C51812dc3A010C7d01b50e0d17dc79C8"), "latest")
		require.NoError(t, err)
		require.NotNil(t, response.Error)
		require.Nil(t, response.Result)
		require.Equal(t, invalidParamsErrorCode, response.Error.Code)
		require.Equal(t, "too many arguments, want at most 0", response.Error.Message)
	}
}

func Test_WebSocketsRequest(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}
	ctx := context.Background()
	setup()
	defer teardown()

	acc := common.HexToAddress(operations.DefaultSequencerAddress)

	for _, network := range networks {
		log.Infof("Network %s", network.Name)

		client, err := ethclient.Dial(network.URL)
		require.NoError(t, err)

		expectedBalance, err := client.BalanceAt(ctx, acc, nil)
		require.NoError(t, err)

		wsConn, _, err := websocket.DefaultDialer.Dial(network.WebSocketURL, nil)
		require.NoError(t, err)

		receivedMessages := make(chan []byte)
		go func() {
			for {
				_, message, err := wsConn.ReadMessage()
				require.NoError(t, err)
				receivedMessages <- message
				wsConn.Close()
				break
			}
		}()

		params := []string{acc.String(), "latest"}
		jParam, err := json.Marshal(params)
		require.NoError(t, err)

		req := types.Request{JSONRPC: "2.0", ID: float64(1), Method: "eth_getBalance", Params: jParam}
		jReq, _ := json.Marshal(req)

		err = wsConn.WriteMessage(websocket.TextMessage, jReq)
		require.NoError(t, err)

		receivedMessage := <-receivedMessages

		resp := types.Response{}
		err = json.Unmarshal(receivedMessage, &resp)
		require.NoError(t, err)

		assert.Equal(t, req.JSONRPC, resp.JSONRPC)
		assert.Equal(t, req.ID, resp.ID)
		assert.Nil(t, resp.Error)
		assert.NotNil(t, resp.Result)

		result := ""
		err = json.Unmarshal(resp.Result, &result)
		require.NoError(t, err)

		str := strings.TrimPrefix(result, "0x")
		balance := hex.DecodeBig(str)
		require.NoError(t, err)

		assert.Equal(t, expectedBalance.String(), balance.String())
	}
}

func Test_WebSocketsSubscription(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}
	setup()
	defer teardown()

	for _, network := range networks {
		log.Infof("Network %s", network.Name)

		wsConn, _, err := websocket.DefaultDialer.Dial(network.WebSocketURL, nil)
		require.NoError(t, err)

		receivedMessages := make(chan []byte)
		go func() {
			for {
				_, message, err := wsConn.ReadMessage()
				require.NoError(t, err)
				receivedMessages <- message
				break
			}
		}()

		params := []string{"newHeads"}
		jParam, err := json.Marshal(params)
		require.NoError(t, err)

		req := types.Request{JSONRPC: "2.0", ID: float64(1), Method: "eth_subscribe", Params: jParam}
		jReq, _ := json.Marshal(req)

		err = wsConn.WriteMessage(websocket.TextMessage, jReq)
		require.NoError(t, err)

		subscriptionMessage := <-receivedMessages

		resp := types.Response{}
		err = json.Unmarshal(subscriptionMessage, &resp)
		require.NoError(t, err)

		assert.Equal(t, req.JSONRPC, resp.JSONRPC)
		assert.Equal(t, req.ID, resp.ID)
		assert.Nil(t, resp.Error)
		assert.NotNil(t, resp.Result)

		subscription := ""
		err = json.Unmarshal(resp.Result, &subscription)
		require.NoError(t, err)

		assert.NotEmpty(t, subscription)

		const numberOfBlocks = 3

		go func() {
			for i := 0; i <= numberOfBlocks; i++ {
				_, message, err := wsConn.ReadMessage()
				require.NoError(t, err)
				receivedMessages <- message
			}
			wsConn.Close()
		}()

		client := operations.MustGetClient(network.URL)
		auth := operations.MustGetAuth(network.PrivateKey, network.ChainID)
		for i := 0; i <= numberOfBlocks; i++ {
			tx, err := createTX(client, auth, toAddress, big.NewInt(1000000000))
			require.NoError(t, err)
			err = operations.WaitTxToBeMined(context.Background(), client, tx, operations.DefaultTimeoutTxToBeMined)
			require.NoError(t, err)
		}

		for i := 0; i <= numberOfBlocks; i++ {
			receivedMessage := <-receivedMessages
			resp := types.SubscriptionResponse{}

			err = json.Unmarshal(receivedMessage, &resp)
			require.NoError(t, err)

			assert.Equal(t, req.JSONRPC, resp.JSONRPC)
			assert.Equal(t, "eth_subscription", resp.Method)
			assert.Equal(t, subscription, resp.Params.Subscription)

			block := map[string]interface{}{}
			err = json.Unmarshal(resp.Params.Result, &block)
			require.NoError(t, err)
			assert.NotEmpty(t, block["hash"].(string))
		}
	}
}

func Test_RevertOnConstructorTransaction(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}
	setup()
	defer teardown()

	ctx := context.Background()

	for _, network := range networks {
		log.Infof("Network %s", network.Name)

		client := operations.MustGetClient(network.URL)
		auth := operations.MustGetAuth(network.PrivateKey, network.ChainID)

		auth.GasLimit = 1000000

		_, scTx, _, err := Revert.DeployRevert(auth, client)
		require.NoError(t, err)

		err = operations.WaitTxToBeMined(ctx, client, scTx, operations.DefaultTimeoutTxToBeMined)
		errMsg := err.Error()
		prefix := "transaction has failed, reason: execution reverted: Today is not juernes"
		hasPrefix := strings.HasPrefix(errMsg, prefix)
		require.True(t, hasPrefix)

		receipt, err := client.TransactionReceipt(ctx, scTx.Hash())
		require.NoError(t, err)

		assert.Equal(t, receipt.Status, ethTypes.ReceiptStatusFailed)

		msg := ethereum.CallMsg{
			From: auth.From,
			To:   scTx.To(),
			Gas:  scTx.Gas(),

			Value: scTx.Value(),
			Data:  scTx.Data(),
		}
		result, err := client.CallContract(ctx, msg, receipt.BlockNumber)
		require.NotNil(t, err)
		require.Nil(t, result)
		rpcErr := err.(rpc.Error)
		assert.Equal(t, 3, rpcErr.ErrorCode())
		assert.Equal(t, "execution reverted: Today is not juernes", rpcErr.Error())

		dataErr := err.(rpc.DataError)
		data := dataErr.ErrorData().(string)
		decodedData := hex.DecodeBig(data)
		unpackedData, err := abi.UnpackRevert(decodedData.Bytes())
		require.NoError(t, err)
		assert.Equal(t, "Today is not juernes", unpackedData)
	}
}

func Test_RevertOnSCCallTransaction(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}
	setup()
	defer teardown()

	ctx := context.Background()

	for _, network := range networks {
		log.Infof("Network %s", network.Name)

		client := operations.MustGetClient(network.URL)
		auth := operations.MustGetAuth(network.PrivateKey, network.ChainID)

		auth.GasLimit = 1000000

		_, scTx, sc, err := Revert2.DeployRevert2(auth, client)
		require.NoError(t, err)

		err = operations.WaitTxToBeMined(ctx, client, scTx, operations.DefaultTimeoutTxToBeMined)
		require.NoError(t, err)

		tx, err := sc.GenerateError(auth)
		require.NoError(t, err)

		err = operations.WaitTxToBeMined(ctx, client, tx, operations.DefaultTimeoutTxToBeMined)
		errMsg := err.Error()
		prefix := "transaction has failed, reason: execution reverted: Today is not juernes"
		hasPrefix := strings.HasPrefix(errMsg, prefix)
		require.True(t, hasPrefix)

		receipt, err := client.TransactionReceipt(ctx, tx.Hash())
		require.NoError(t, err)

		assert.Equal(t, receipt.Status, ethTypes.ReceiptStatusFailed)

		msg := ethereum.CallMsg{
			From: auth.From,
			To:   tx.To(),
			Gas:  tx.Gas(),

			Value: tx.Value(),
			Data:  tx.Data(),
		}
		result, err := client.CallContract(ctx, msg, receipt.BlockNumber)
		require.NotNil(t, err)
		require.Nil(t, result)
		rpcErr := err.(rpc.Error)
		assert.Equal(t, 3, rpcErr.ErrorCode())
		assert.Equal(t, "execution reverted: Today is not juernes", rpcErr.Error())

		dataErr := err.(rpc.DataError)
		data := dataErr.ErrorData().(string)
		decodedData := hex.DecodeBig(data)
		unpackedData, err := abi.UnpackRevert(decodedData.Bytes())
		require.NoError(t, err)
		assert.Equal(t, "Today is not juernes", unpackedData)
	}
}
