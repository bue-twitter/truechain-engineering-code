package etrue

import (
	"math/big"
	"time"
	"github.com/truechain/truechain-engineering-code/common"
	"github.com/truechain/truechain-engineering-code/consensus"
	"github.com/truechain/truechain-engineering-code/core"
	"github.com/truechain/truechain-engineering-code/core/state"
	"github.com/truechain/truechain-engineering-code/core/types"
	"github.com/truechain/truechain-engineering-code/ethdb"
	"github.com/truechain/truechain-engineering-code/event"
	"github.com/truechain/truechain-engineering-code/log"
	"github.com/truechain/truechain-engineering-code/params"
	"github.com/truechain/truechain-engineering-code/accounts"
	"github.com/truechain/truechain-engineering-code/core/vm"
	"github.com/truechain/truechain-engineering-code/core/fastchain"
	"github.com/truechain/truechain-engineering-code/etrue/truechain"
	"github.com/truechain/truechain-engineering-code/crypto"
	"github.com/truechain/truechain-engineering-code/crypto/ecies"
	"crypto/ecdsa"
	"sync"
	"bytes"
	"fmt"
	"crypto/rand"
)

const (
	VoteAgree = iota		//vote agree
	VoteAgreeAgainst  		//vote against
)

type PbftAgent struct {
	config *params.ChainConfig
	chain   *fastchain.FastBlockChain

	engine consensus.Engine
	eth     Backend
	signer types.Signer
	current *AgentWork

	currentMu sync.Mutex
	mux          *event.TypeMux
	agentFeed       event.Feed
	scope        event.SubscriptionScope

	snapshotMu    sync.RWMutex
	snapshotState *state.StateDB
	snapshotBlock *types.FastBlock

	committeeActionCh  chan PbftCommitteeActionEvent
	committeeSub event.Subscription

	eventMux      *event.TypeMux
	PbftNodeSub *event.TypeMuxSubscription
	election	*Election

	CommitteeMembers	[]*CommitteeMember
}

// PbftVoteSigns is a PbftVoteSign slice type for basic sorting.
type PbftVoteSigns []*types.PbftSign

type PbftNode struct {
	NodeIP string
	NodePort uint
	CoinBase common.Address
	PublicKey *ecdsa.PublicKey
}

type  CryNodeInfo struct {
	InfoByte	[]byte	//签名前
	Sign 		[]byte	//签名后
}

type PbftAction struct {
	Id *big.Int		//committee times
	action int
}

type AgentWork struct {
	config *params.ChainConfig
	signer types.Signer

	state     *state.StateDB // apply state changes here
	tcount    int            // tx count in cycle
	gasPool   *fastchain.GasPool  // available gas used to pack transactions

	Block *types.FastBlock // the new block

	header   *types.FastHeader
	txs      []*types.Transaction
	receipts []*types.Receipt

	createdAt time.Time
}

type Backend interface {
	AccountManager() *accounts.Manager
	FastBlockChain() *fastchain.FastBlockChain
	TxPool() *core.TxPool
	ChainDb() ethdb.Database
}

type NewPbftNodeEvent struct{ cryNodeInfo *CryNodeInfo}

type  BlockAndSign struct{
	Block *types.FastBlock
	Sign  *types.PbftSign
}
var
(	privateKey *ecdsa.PrivateKey
	pbftNode PbftNode
	pks []*ecdsa.PublicKey
	voteResult map[*big.Int]int	= make(map[*big.Int]int)
)

func NewPbftAgent(eth Backend, config *params.ChainConfig,mux *event.TypeMux, engine consensus.Engine) *PbftAgent {
	self := &PbftAgent{
		config:         	config,
		engine:         	engine,
		eth:            	eth,
		mux:            	mux,
		chain:          	eth.FastBlockChain(),
		committeeActionCh:	make(chan PbftCommitteeActionEvent, 3),
		election:nil,
	}
	fastBlock :=self.chain.CurrentFastBlock()
	_,self.CommitteeMembers =self.election.GetCommittee(fastBlock.Header().Number,fastBlock.Header().Hash())

	// Subscribe events from blockchain
	//self.committeeSub = self.chain.SubscribeChainHeadEvent(self.committeeActionCh)
	self.committeeSub = self.election.SubscribeCommitteeActionEvent(self.committeeActionCh)
	go self.loop()
	return self
}

func (self *PbftAgent) loop(){
	fmt.Println("loop...")
	for {
		select {
		// Handle ChainHeadEvent
		case ch := <-self.committeeActionCh:
			if ch.pbftAction.action ==types.CommitteeStart{
				//Actions(committeeAction)
			}else if ch.pbftAction.action ==types.CommitteeStop{

			}else if ch.pbftAction.action ==types.CommitteeSwitchover{
				self.SendPbftNode()//broad nodeInfo of self
				self.Start()//receive nodeInfo from other member
			}
		}
	}
}

func (pbftAgent *PbftAgent)  SendPbftNode()	*CryNodeInfo{
	var nodeInfos [][]byte
	nodeByte,_ :=truechain.ToByte(pbftNode)

	for _,committeeMember := range pbftAgent.CommitteeMembers{
		encryptMsg,err :=ecies.Encrypt(rand.Reader,ecies.ImportECDSAPublic(committeeMember.pubkey),nodeByte, nil, nil)
		if err != nil{
			return nil
		}
		nodeInfos =append(nodeInfos,encryptMsg)
	}
	infoByte,_ := truechain.ToByte(nodeInfos)
	sigInfo,err :=crypto.Sign(infoByte, privateKey)
	if err != nil{
		log.Info("sign error")
	}
	cryNodeInfo :=&CryNodeInfo{infoByte,sigInfo}
	pbftAgent.eventMux.Post(NewPbftNodeEvent{cryNodeInfo})

	return cryNodeInfo
}

func (pbftAgent *PbftAgent) Start() {
	// broadcast mined blocks
	pbftAgent.PbftNodeSub = pbftAgent.eventMux.Subscribe(NewPbftNodeEvent{})
	go pbftAgent.handle()
}

func  (pbftAgent *PbftAgent) handle(){
	for obj := range pbftAgent.PbftNodeSub.Chan() {
		switch cryNodeInfo := obj.Data.(type) {
		case CryNodeInfo:
			pbftAgent.ReceivePbftNode(cryNodeInfo)
		}
	}
}

func (pbftAgent *PbftAgent)  ReceivePbftNode(cryNodeInfo CryNodeInfo) *PbftNode {
	hash:= cryNodeInfo.InfoByte
	sig := cryNodeInfo.Sign
	var node *PbftNode

	pubKey,err :=crypto.SigToPub(hash,sig)
	if err != nil{
		log.Info("SigToPub error.")
		return nil
	}

	verifyFlag := false
	for _, committeeMembers:= range pbftAgent.CommitteeMembers{
		if !bytes.Equal(crypto.FromECDSAPub(pubKey), crypto.FromECDSAPub(committeeMembers.pubkey)) {
			continue
		}else{
			verifyFlag = true
		}
	}
	if !verifyFlag{
		log.Info("publicKey is not exist.")
		return nil
	}
	var nodeInfos [][]byte
	truechain.FromByte(hash,nodeInfos)
	priKey :=ecies.ImportECDSA(privateKey)//ecdsa-->ecies
	for _,info := range nodeInfos{
		encryptMsg,err :=priKey.Decrypt(info, nil, nil)
		if err != nil{
			truechain.FromByte(encryptMsg,node)
			return node
		}
	}
	return nil
}

//generateBlock and broadcast
func  (self * PbftAgent)  FetchBlock() (*types.FastBlock,error){
	var fastBlock  *types.FastBlock

	tstart := time.Now()
	parent := self.chain.CurrentBlock()

	tstamp := tstart.Unix()
	if parent.Time().Cmp(new(big.Int).SetInt64(tstamp)) >= 0 {
		tstamp = parent.Time().Int64() + 1
	}
	// this will ensure we're not going off too far in the future
	if now := time.Now().Unix(); tstamp > now+1 {
		wait := time.Duration(tstamp-now) * time.Second
		log.Info("generateFastBlock too far in the future", "wait", common.PrettyDuration(wait))
		time.Sleep(wait)
	}

	num := parent.Number()
	header := &types.FastHeader{
		ParentHash: parent.Hash(),
		Number:     num.Add(num, common.Big1),
		GasLimit:   fastchain.FastCalcGasLimit(parent),
		Time:       big.NewInt(tstamp),
	}

	if err := self.engine.PrepareFast(self.chain, header); err != nil {
		log.Error("Failed to prepare header for generateFastBlock", "err", err)
		return	fastBlock,err
	}
	// Create the current work task and check any fork transitions needed
	err := self.makeCurrent(parent, header)
	work := self.current

	pending, err := self.eth.TxPool().Pending()
	if err != nil {
		log.Error("Failed to fetch pending transactions", "err", err)
		return	fastBlock,err
	}
	txs := types.NewTransactionsByPriceAndNonce(self.current.signer, pending)
	work.commitTransactions(self.mux, txs, self.chain)

	//  padding Header.Root, TxHash, ReceiptHash.
	// Create the new block to seal with the consensus engine
	if fastBlock, err = self.engine.FinalizeFast(self.chain, header, work.state, work.txs, work.receipts); err != nil {
		log.Error("Failed to finalize block for sealing", "err", err)
		return	fastBlock,err
	}
	voteSign := &types.PbftSign{
		Result: VoteAgree,
		FastHeight:fastBlock.Header().Number,
		FastHash:fastBlock.Hash(),
	}
	msgByte :=voteSign.PrepareData()
	hash :=truechain.RlpHash(msgByte)
	voteSign.Sign,err =crypto.Sign(hash[:], privateKey)
	if err != nil{
		log.Info("sign error")
	}
	blockAndSign := &BlockAndSign{
		fastBlock,
		voteSign,
	}
	//broadcast blockAndSign
	self.mux.Post(core.NewMinedFastBlockEvent{blockAndSign})
	return	fastBlock,nil
}

func (self * PbftAgent) VerifyFastBlock(fb *types.FastBlock) error{
	bc := self.chain
	err :=bc.Engine().VerifyFastHeader(bc, fb.Header(),true)
	if err == nil{
		err = bc.Validator().ValidateBody(fb)
	}
	if err != nil{
		return err
	}
	var parent *types.FastBlock
	parent = bc.GetBlock(fb.ParentHash(), fb.NumberU64()-1)

	//abort, results  :=bc.Engine().VerifyPbftFastHeader(bc, fb.Header(),parent.Header())

	state, err := bc.State()
	if err != nil{
		return err
	}
	receipts, _, usedGas, err := bc.Processor().Process(fb, state, vm.Config{})//update
	if err != nil{
		return err
	}
	err = bc.Validator().ValidateState(fb, parent, state, receipts, usedGas)
	if err != nil{
		return err
	}
	/*// Write the block to the chain and get the status.
	status, err := bc.WriteBlockWithState(fb, receipts, state) //update
	if err != nil{
		return err
	}
	if status  == fastchain.CanonStatTy{
		log.Debug("Inserted new block", "number", fb.Number(), "hash", fb.Hash(), "uncles", 0,
			"txs", len(fb.Transactions()), "gas", fb.GasUsed(), "elapsed", "")
	}*/
	return nil

}

//verify the sign , insert chain  and  broadcast the signs
func  (self *PbftAgent)  ComplateSign(voteSigns []*types.PbftSign){
	var FastHeight *big.Int
	for _,voteSign := range voteSigns{
		FastHeight =voteSign.FastHeight
		if voteSign.Result == VoteAgreeAgainst{
			continue
		}
		msg :=voteSign.PrepareData()
		pubKey,err :=crypto.SigToPub(msg,voteSign.Sign)
		if err != nil{
			log.Info("SigToPub error.")
			panic(err)
		}
		for _,pk := range pks {
			if bytes.Equal(crypto.FromECDSAPub(pubKey), crypto.FromECDSAPub(pk)) {
				val,ok:=voteResult[voteSign.FastHeight]
				if ok{
					voteResult[voteSign.FastHeight]=val+1
				}else{
					voteResult[voteSign.FastHeight]=1
				}
				break;
			}
		}
	}
	if voteResult[FastHeight] > 2*len(pks)/3{
		var fastBlocks []*types.FastBlock
		_,err :=self.chain.InsertChain(fastBlocks)
		if err != nil{
			panic(err)
		}

		self.agentFeed.Send(core.PbftVoteSignEvent{voteSigns})
	}
}

func (self *PbftAgent) makeCurrent(parent *types.FastBlock, header *types.FastHeader) error {
	state, err := self.chain.StateAt(parent.Root())
	if err != nil {
		return err
	}
	work := &AgentWork{
		config:    self.config,
		signer:    types.NewEIP155Signer(self.config.ChainID),
		state:     state,
		header:    header,
		createdAt: time.Now(),
	}
	// Keep track of transactions which return errors so they can be removed
	work.tcount = 0
	self.current = work
	return nil
}

func (env *AgentWork) commitTransactions(mux *event.TypeMux, txs *types.TransactionsByPriceAndNonce,
	bc *fastchain.FastBlockChain) {
	if env.gasPool == nil {
		env.gasPool = new(fastchain.GasPool).AddGas(env.header.GasLimit)
	}

	var coalescedLogs []*types.Log

	for {
		// If we don't have enough gas for any further transactions then we're done
		if env.gasPool.Gas() < params.TxGas {
			log.Trace("Not enough gas for further transactions", "have", env.gasPool, "want", params.TxGas)
			break
		}
		// Retrieve the next transaction and abort if all done
		tx := txs.Peek()
		if tx == nil {
			break
		}
		// Error may be ignored here. The error has already been checked
		// during transaction acceptance is the transaction pool.
		//
		// We use the eip155 signer regardless of the current hf.
		from, _ := types.Sender(env.signer, tx)
		// Check whether the tx is replay protected. If we're not in the EIP155 hf
		// phase, start ignoring the sender until we do.
		if tx.Protected() && !env.config.IsEIP155(env.header.Number) {
			log.Trace("Ignoring reply protected transaction", "hash", tx.Hash(), "eip155", env.config.EIP155Block)

			txs.Pop()
			continue
		}
		// Start executing the transaction
		env.state.Prepare(tx.Hash(), common.Hash{}, env.tcount)

		err, logs := env.commitTransaction(tx, bc,env.gasPool)
		switch err {
		case core.ErrGasLimitReached:
			// Pop the current out-of-gas transaction without shifting in the next from the account
			log.Trace("Gas limit exceeded for current block", "sender", from)
			txs.Pop()

		case core.ErrNonceTooLow:
			// New head notification data race between the transaction pool and miner, shift
			log.Trace("Skipping transaction with low nonce", "sender", from, "nonce", tx.Nonce())
			txs.Shift()

		case core.ErrNonceTooHigh:
			// Reorg notification data race between the transaction pool and miner, skip account =
			log.Trace("Skipping account with hight nonce", "sender", from, "nonce", tx.Nonce())
			txs.Pop()

		case nil:
			// Everything ok, collect the logs and shift in the next transaction from the same account
			coalescedLogs = append(coalescedLogs, logs...)
			env.tcount++
			txs.Shift()

		default:
			// Strange error, discard the transaction and get the next in line (note, the
			// nonce-too-high clause will prevent us from executing in vain).
			log.Debug("Transaction failed, account skipped", "hash", tx.Hash(), "err", err)
			txs.Shift()
		}
	}

	if len(coalescedLogs) > 0 || env.tcount > 0 {
		// make a copy, the state caches the logs and these logs get "upgraded" from pending to mined
		// logs by filling in the block hash when the block was mined by the local miner. This can
		// cause a race condition if a log was "upgraded" before the PendingLogsEvent is processed.
		cpy := make([]*types.Log, len(coalescedLogs))
		for i, l := range coalescedLogs {
			cpy[i] = new(types.Log)
			*cpy[i] = *l
		}
		go func(logs []*types.Log, tcount int) {
			if len(logs) > 0 {
				mux.Post(core.PendingLogsEvent{Logs: logs})
			}
			if tcount > 0 {
				mux.Post(core.PendingStateEvent{})
			}
		}(cpy, env.tcount)
	}
}

func (env *AgentWork) commitTransaction(tx *types.Transaction, bc *fastchain.FastBlockChain,  gp *fastchain.GasPool) (error, []*types.Log) {
	snap := env.state.Snapshot()

	receipt, _, err := fastchain.FastApplyTransaction(env.config, bc, gp, env.state, env.header, tx, &env.header.GasUsed, vm.Config{})
	if err != nil {
		env.state.RevertToSnapshot(snap)
		return err, nil
	}
	env.txs = append(env.txs, tx)
	env.receipts = append(env.receipts, receipt)

	return nil, receipt.Logs
}



// SubscribeNewPbftVoteSignEvent registers a subscription of PbftVoteSignEvent and
// starts sending event to the given channel.
func (self * PbftAgent) SubscribeNewPbftVoteSignEvent(ch chan<- core.PbftVoteSignEvent) event.Subscription {
	return self.scope.Track(self.agentFeed.Subscribe(ch))
}

// Stop terminates the PbftAgent.
func (self * PbftAgent) Stop() {
	// Unsubscribe all subscriptions registered from agent
	self.scope.Close()
}