package dpos

import (
	"bytes"
	"encoding/binary"
	"errors"
	_ "fmt"
	"math/big"
	"sync"
	"time"

	"github.com/boker/go-ethereum/accounts"
	"github.com/boker/go-ethereum/common"
	"github.com/boker/go-ethereum/consensus"
	"github.com/boker/go-ethereum/consensus/misc"
	"github.com/boker/go-ethereum/core/state"
	"github.com/boker/go-ethereum/core/types"
	"github.com/boker/go-ethereum/crypto"
	"github.com/boker/go-ethereum/crypto/sha3"
	"github.com/boker/go-ethereum/ethdb"
	"github.com/boker/go-ethereum/include"
	"github.com/boker/go-ethereum/log"
	"github.com/boker/go-ethereum/params"
	"github.com/boker/go-ethereum/rlp"
	"github.com/boker/go-ethereum/rpc"
	"github.com/boker/go-ethereum/trie"
	lru "github.com/hashicorp/golang-lru"
)

var (
	errUnknownBlock               = errors.New("unknown block")                               //未知区块
	errMissingVanity              = errors.New("extra-data 32 byte vanity prefix missing")    //如果一个块的额外数据段小于存储签名者所必需的32字节，则返回errMissingVanity
	errMissingSignature           = errors.New("extra-data 65 byte suffix signature missing") //如果块的额外数据部分没有包含一个65字节的secp256k1签名，则返回errMissingSignature
	errInvalidMixDigest           = errors.New("non-zero mix digest")                         // 如果块的混合摘要不为零，则返回errInvalidMixDigest。
	errInvalidUncleHash           = errors.New("non empty uncle hash")                        //叔块Hash未定义（Dpos下不存在叔块）
	errInvalidDifficulty          = errors.New("invalid difficulty")                          //难度未定义
	ErrInvalidTimestamp           = errors.New("invalid timestamp")                           //出块时间不正确
	ErrWaitForPrevBlock           = errors.New("wait for last block arrived")                 //等待最后一个区块到达
	ErrMintFutureBlock            = errors.New("mint the future block")                       //根据时间计算是一个未来的区块
	ErrMismatchSignerAndValidator = errors.New("mismatch block signer and validator")         //签名者和区块头中的验证者不是同一个
	ErrInvalidBlockProducer       = errors.New("invalid block producer")                      //这个区块不应该由这个验证者出（出块有顺序，不能乱出块的）
	ErrInvalidTokenNoder          = errors.New("invalid block token noder")                   //这个区块不应该由这个验证者出（出块有顺序，不能乱出块的）
	ErrNilBlockHeader             = errors.New("nil block header returned")                   //区块头为空
)
var (
	uncleHash = types.CalcUncleHash(nil) // Always Keccak256(RLP([])) as uncles are meaningless outside of PoW.
)

type Dpos struct {
	config               *params.DposConfig //共识引擎的配置参数
	db                   ethdb.Database     //数据库对象
	signer               common.Address     //签名者地址
	signFn               SignerFn           //签名处理函数
	signatures           *lru.ARCCache      //最近的块签名加快采矿
	confirmedBlockHeader *types.Header
	mu                   sync.RWMutex
	stop                 chan bool
}

type SignerFn func(accounts.Account, []byte) ([]byte, error)

func sigHash(header *types.Header) (hash common.Hash) {
	hasher := sha3.NewKeccak256()

	rlp.Encode(hasher, []interface{}{
		header.ParentHash,
		header.UncleHash,
		header.Validator,
		header.Coinbase,
		header.Root,
		header.TxHash,
		header.ReceiptHash,
		header.Bloom,
		header.Difficulty,
		header.Number,
		header.GasLimit,
		header.GasUsed,
		header.Time,
		header.Extra[:len(header.Extra)-65], // Yes, this will panic if extra is too short
		header.MixDigest,
		header.Nonce,
		header.DposContext.Root(),
	})
	hasher.Sum(hash[:0])
	return hash
}

//创建一个新的Dpos对象
func New(config *params.DposConfig, db ethdb.Database) *Dpos {
	signatures, _ := lru.NewARC(include.InmemorySignatures)
	return &Dpos{
		config:     config,
		db:         db,
		signatures: signatures,
	}
}

//根据区块头得到验证者
func (d *Dpos) Author(header *types.Header) (common.Address, error) {
	return header.Validator, nil
}

//校验区块头
func (d *Dpos) VerifyHeader(chain consensus.ChainReader, header *types.Header, seal bool) error {
	return d.verifyHeader(chain, header, nil)
}

func (d *Dpos) verifyHeader(chain consensus.ChainReader, header *types.Header, parents []*types.Header) error {

	//判断区块头中区块号是否为空
	if header.Number == nil {
		return errUnknownBlock
	}
	number := header.Number.Uint64()

	//用区块头中的时间和当前时间对比，如果大于当前时间则属于未来的区块（还没有出现的区块），报错
	if header.Time.Cmp(big.NewInt(time.Now().Unix())) > 0 {
		return consensus.ErrFutureBlock
	}

	//检测区块头中的扩展数据长度是否大于扩展签名头长度（32）
	if len(header.Extra) < include.ExtraVanity {
		return errMissingVanity
	}

	//检测区块头中的扩展数据长度是否大于扩展签名长度头+扩展签名长度尾 = 32 + 65 = 97
	if len(header.Extra) < include.ExtraVanity+include.ExtraSeal {
		return errMissingSignature
	}

	// Ensure that the mix digest is zero as we don't have fork protection currently
	// 确保混合摘要为零，因为我们目前没有叉保护
	if header.MixDigest != (common.Hash{}) {
		return errInvalidMixDigest
	}

	//检测区块头难度是否为1（由于采用的是DPOS，所以难度一定为1[此处在拼接区块头的时候有设置]）
	if header.Difficulty.Uint64() != 1 {
		return errInvalidDifficulty
	}

	//区块头是否包含叔块Hash（DPOS不应该包含叔块）
	if header.UncleHash != uncleHash {
		return errInvalidUncleHash
	}

	//检测硬分叉的特殊字段判断是否是硬分叉
	if err := misc.VerifyForkHashes(chain.Config(), header, false); err != nil {
		return err
	}

	//定义父节点区块头
	var parent *types.Header
	if len(parents) > 0 {
		parent = parents[len(parents)-1]
	} else {
		parent = chain.GetHeader(header.ParentHash, number-1)
	}
	if parent == nil || parent.Number.Uint64() != number-1 || parent.Hash() != header.ParentHash {
		return consensus.ErrUnknownAncestor
	}
	if parent.Time.Uint64()+uint64(include.ProducerInterval) > header.Time.Uint64() {
		return ErrInvalidTimestamp
	}
	return nil
}

//验证区块头
func (d *Dpos) VerifyHeaders(chain consensus.ChainReader, headers []*types.Header, seals []bool) (chan<- struct{}, <-chan error) {
	abort := make(chan struct{})
	results := make(chan error, len(headers))

	go func() {
		for i, header := range headers {
			err := d.verifyHeader(chain, header, headers[:i])
			select {
			case <-abort:
				return
			case results <- err:
			}
		}
	}()
	return abort, results
}

//验证叔块，如果存在叔块则返回错误
func (d *Dpos) VerifyUncles(chain consensus.ChainReader, block *types.Block) error {
	if len(block.Uncles()) > 0 {
		return errors.New("uncles not allowed")
	}
	return nil
}

func (d *Dpos) VerifySeal(chain consensus.ChainReader, header *types.Header) error {

	return d.verifySeal(chain, header, nil)
}

func (d *Dpos) verifySeal(chain consensus.ChainReader, header *types.Header, parents []*types.Header) error {

	//判断是否是创始区块
	number := header.Number.Uint64()
	if number == 0 {
		return errUnknownBlock
	}

	//得到父区块信息
	var parent *types.Header
	if len(parents) > 0 {
		parent = parents[len(parents)-1]
	} else {
		parent = chain.GetHeader(header.ParentHash, number-1)
	}

	//根据父区块创建一个新的Dpos对象
	dposContext, err := types.NewDposContextFromProto(d.db, parent.DposContext)
	if err != nil {
		return err
	}

	//根据Dpos对象创建一个周期对象
	producer, err := dposContext.GetProducer(header.Time.Int64())
	if err != nil {
		return err
	}

	//验证区块签名者
	if err := d.verifyBlockSigner(producer, header); err != nil {
		return err
	}
	return d.updateConfirmedBlockHeader(chain)
}

//验证区块签名
func (d *Dpos) verifyBlockSigner(producer common.Address, header *types.Header) error {

	//根据包头得到签名者
	signer, err := ecrecover(header, d.signatures)
	if err != nil {
		return err
	}

	//判断签名者和验证者是否是一个人
	if bytes.Compare(signer.Bytes(), producer.Bytes()) != 0 {
		return ErrInvalidBlockProducer
	}

	//判断签名者和区块头中的验证者是否是同一个人
	if bytes.Compare(signer.Bytes(), header.Validator.Bytes()) != 0 {
		return ErrMismatchSignerAndValidator
	}
	return nil
}

//更新确认的区块头
func (d *Dpos) updateConfirmedBlockHeader(chain consensus.ChainReader) error {

	//判断确认区块头为空
	if d.confirmedBlockHeader == nil {
		header, err := d.loadConfirmedBlockHeader(chain)
		if err != nil {
			header = chain.GetHeaderByNumber(0)
			if header == nil {
				return err
			}
		}
		d.confirmedBlockHeader = header
	}

	//获取当前区块头
	curHeader := chain.CurrentHeader()
	epoch := int64(-1)
	validatorMap := make(map[common.Address]bool)
	for d.confirmedBlockHeader.Hash() != curHeader.Hash() && d.confirmedBlockHeader.Number.Uint64() < curHeader.Number.Uint64() {

		//得到当前的周期循环数
		curEpoch := curHeader.Time.Int64() / include.EpochInterval

		//当前周期不等于初始-1的周期
		if curEpoch != epoch {
			epoch = curEpoch
			validatorMap = make(map[common.Address]bool)
		}

		//当前区块头序号-已经确认的区块头序号 < 共识确认验证者数量 - 当前验证者数量 (此处用于处理重复确认)
		if curHeader.Number.Int64()-d.confirmedBlockHeader.Number.Int64() < int64(include.ConsensusSize-len(validatorMap)) {
			log.Debug("Dpos fast return", "current", curHeader.Number.String(), "confirmed", d.confirmedBlockHeader.Number.String(), "witnessCount", len(validatorMap))
			return nil
		}

		//
		validatorMap[curHeader.Validator] = true
		if len(validatorMap) >= include.ConsensusSize {
			d.confirmedBlockHeader = curHeader
			if err := d.storeConfirmedBlockHeader(d.db); err != nil {
				return err
			}
			log.Debug("Dpos set confirmed block header success", "currentHeader", curHeader.Number.String())
			return nil
		}
		curHeader = chain.GetHeaderByHash(curHeader.ParentHash)
		if curHeader == nil {
			return ErrNilBlockHeader
		}
	}
	return nil
}

//加载确认区块头
func (s *Dpos) loadConfirmedBlockHeader(chain consensus.ChainReader) (*types.Header, error) {
	key, err := s.db.Get(include.ConfirmedBlockHead)
	if err != nil {
		return nil, err
	}
	header := chain.GetHeaderByHash(common.BytesToHash(key))
	if header == nil {
		return nil, ErrNilBlockHeader
	}
	return header, nil
}

//确认区块头放入数据库池中
func (s *Dpos) storeConfirmedBlockHeader(db ethdb.Database) error {
	return db.Put(include.ConfirmedBlockHead, s.confirmedBlockHeader.Hash().Bytes())
}

//拼接区块头信息
func (d *Dpos) Prepare(chain consensus.ChainReader, header *types.Header) error {

	//设置区块头中的Nonce字段，防止双花攻击
	header.Nonce = types.BlockNonce{}
	number := header.Number.Uint64()
	if len(header.Extra) < include.ExtraVanity {
		header.Extra = append(header.Extra, bytes.Repeat([]byte{0x00}, include.ExtraVanity-len(header.Extra))...)
	}

	//设置区块头的扩展字段信息
	header.Extra = header.Extra[:include.ExtraVanity]
	header.Extra = append(header.Extra, make([]byte, include.ExtraSeal)...)

	//根据区块头得到父区块的信息
	parent := chain.GetHeader(header.ParentHash, number-1)
	if parent == nil {
		return consensus.ErrUnknownAncestor
	}

	//设置区块难度(此处恒定为1)
	header.Difficulty = d.CalcDifficulty(chain, header.Time.Uint64(), parent)

	//设置区块头的验证者的签名
	header.Validator = d.signer
	return nil
}

//累计奖励
func AccumulateRewards(config *params.ChainConfig, state *state.StateDB, header *types.Header, uncles []*types.Header) {

	//Bobby的出块奖励数量（11Bobby）
	blockReward := big.NewInt(1)
	blockReward.Mul(include.BobbyUnit, include.BobbyMultiple)

	//设置区块奖励数量并累积到帐号中
	reward := new(big.Int).Set(blockReward)
	state.AddBalance(header.Coinbase, reward)

	//给指定账号奖励，此账号用于分配通证给其它用户(16.5Bobby)
	blockTransfer := big.NewInt(1)
	blockTransfer.Mul(include.TransferUnit, include.TransferMultiple)

	//给矿工奖励
	transferReward := new(big.Int).Set(blockTransfer)
	state.AddBalance(header.Coinbase, transferReward)
}

//将交易放入到区块中
func (d *Dpos) Finalize(chain consensus.ChainReader,
	header *types.Header,
	state *state.StateDB,
	txs []*types.Transaction,
	uncles []*types.Header,
	receipts []*types.Receipt,
	dposContext *types.DposContext) (*types.Block, error) {

	//累计奖励奖励并修改帐号中的币数量
	AccumulateRewards(chain.Config(), state, header, uncles)
	header.Root = state.IntermediateRoot(chain.Config().IsEIP158(header.Number))

	parent := chain.GetHeaderByHash(header.ParentHash)
	if include.TimeOfFirstBlock == 0 {
		if firstBlockHeader := chain.GetHeaderByNumber(1); firstBlockHeader != nil {
			include.TimeOfFirstBlock = firstBlockHeader.Time.Int64()
		}
	}

	//判断当前出块节点情况

	//得到创世区块
	/*genesis := chain.GetHeaderByNumber(0)
	err := epochContext.tryElect(genesis, parent)
	if err != nil {
		return nil, fmt.Errorf("got error when elect next epoch, err: %s", err)
	}*/

	//更新MintCnt的默克尔树，并返回一个新区块
	updateMintCnt(parent.Time.Int64(), header.Time.Int64(), header.Validator, dposContext)
	header.DposContext = dposContext.ToProto()
	return types.NewBlock(header, txs, uncles, receipts), nil
}

//检测区块的时间信息
func (d *Dpos) CheckDeadline(lastBlock *types.Block, now int64) error {

	//根据当前时间得到上一个出块时间和下一个出块时间
	prevSlot := PrevSlot(now)
	nextSlot := NextSlot(now)

	//判断最后的区块时间是否大于下一个的区块时间
	if lastBlock.Time().Int64() >= nextSlot {
		return ErrMintFutureBlock
	}

	offset := now % include.EpochInterval
	if offset%include.ProducerInterval != 0 {
		return include.ErrInvalidMintBlockTime
	}

	//当前区块是上一个区块，并且下一个出块时间减当前时间小于1秒（说明可以进行出块了）
	if lastBlock.Time().Int64() == prevSlot || nextSlot-now <= 1 {
		return nil
	}
	return ErrInvalidTimestamp
}

//检测当前区块头中是否是当前的打包节点
func (d *Dpos) CheckProducer(lastBlock *types.Block, now int64) error {

	dposContext, err := types.NewDposContextFromProto(d.db, lastBlock.Header().DposContext)
	if err != nil {
		return err
	}
	producer, err := dposContext.GetProducer(now)
	if err != nil {
		return err
	}
	if (producer == common.Address{}) || bytes.Compare(producer.Bytes(), d.signer.Bytes()) != 0 {
		return ErrInvalidBlockProducer
	}
	return nil
}

//检测当前区块头中是否是当前的打包节点
func (d *Dpos) UpdateProducer(lastBlock *types.Block, producer common.Address) error {

	dposContext, err := types.NewDposContextFromProto(d.db, lastBlock.Header().DposContext)
	if err != nil {
		return err
	}
	producers, err := dposContext.GetEpochTrie()
	if err != nil {
		return err
	}

	//判断当前节点是否在打包节点中
	for _, validator := range producers {
		if validator == producer {
			return nil
		}
	}

	//将出块节点写入
	/*producers = append(producers, producer)
	dposContext.SetValidators(producers)
	for _, validator := range producers {
		dposContext.DelegateTrie().TryUpdate(append(validator.Bytes(), validator.Bytes()...), validator.Bytes())
		dposContext.CandidateTrie().TryUpdate(validator.Bytes(), validator.Bytes())
	}*/

	return nil
}

//检查当前的验证者是否为当前节点
func (d *Dpos) CheckTokenNoder(lastBlock *types.Block, now int64) error {

	if err := d.CheckDeadline(lastBlock, now); err != nil {
		return err
	}
	dposContext, err := types.NewDposContextFromProto(d.db, lastBlock.Header().DposContext)
	if err != nil {
		return err
	}
	producer, err := dposContext.GetTokenNoder(now)
	if err != nil {
		return err
	}
	if (producer == common.Address{}) || bytes.Compare(producer.Bytes(), d.signer.Bytes()) != 0 {
		return ErrInvalidTokenNoder
	}
	return nil
}

//封装区块
func (d *Dpos) Seal(chain consensus.ChainReader, block *types.Block, stop <-chan struct{}) (*types.Block, error) {

	header := block.Header()
	number := header.Number.Uint64()
	if number == 0 {
		return nil, errUnknownBlock
	}
	now := time.Now().Unix()

	//得到下一个区块出块时间-当前时间的差值
	delay := NextSlot(now) - now
	if delay > 0 {
		select {
		case <-stop:
			return nil, nil
		case <-time.After(time.Duration(delay) * time.Second):
		}
	}
	block.Header().Time.SetInt64(time.Now().Unix())

	//对区块进行签名
	sighash, err := d.signFn(accounts.Account{Address: d.signer}, sigHash(header).Bytes())
	if err != nil {
		return nil, err
	}
	copy(header.Extra[len(header.Extra)-include.ExtraSeal:], sighash)
	return block.WithSeal(header), nil
}

//设置难度（恒定为1）
func (d *Dpos) CalcDifficulty(chain consensus.ChainReader, time uint64, parent *types.Header) *big.Int {
	return big.NewInt(1)
}

func (d *Dpos) APIs(chain consensus.ChainReader) []rpc.API {

	return []rpc.API{{
		Namespace: "dpos",
		Version:   "1.0",
		Service:   &API{chain: chain, dpos: d},
		Public:    true,
	}}
}

func (d *Dpos) Authorize(signer common.Address, signFn SignerFn) {

	d.mu.Lock()
	d.signer = signer
	d.signFn = signFn
	d.mu.Unlock()
}

//根据签名头获取到用户账号
func ecrecover(header *types.Header, sigcache *lru.ARCCache) (common.Address, error) {

	//如果已在缓存中，则直接返回
	hash := header.Hash()
	if address, known := sigcache.Get(hash); known {
		return address.(common.Address), nil
	}

	//判断包头扩展字段的长度是否小于扩展字段后缀长度（65）
	if len(header.Extra) < include.ExtraSeal {
		return common.Address{}, errMissingSignature
	}

	//得到公钥
	signature := header.Extra[len(header.Extra)-include.ExtraSeal:]
	pubkey, err := crypto.Ecrecover(sigHash(header).Bytes(), signature)
	if err != nil {
		return common.Address{}, err
	}

	//公钥加密
	var signer common.Address
	copy(signer[:], crypto.Keccak256(pubkey[1:])[12:])
	sigcache.Add(hash, signer)
	return signer, nil
}

//得到区块的上一次生成时间和下一次生成时间
func PrevSlot(now int64) int64 {
	return int64((now-1)/include.ProducerInterval) * include.ProducerInterval
}

func NextSlot(now int64) int64 {
	return int64((now+include.ProducerInterval-1)/include.ProducerInterval) * include.ProducerInterval
}

//修改出块节点出块的数量
func updateMintCnt(parentBlockTime, currentBlockTime int64, validator common.Address, dposContext *types.DposContext) {

	//得到上一个区块的周期数量
	blockCntTrie := dposContext.BlockCntTrie()
	currentEpoch := parentBlockTime / include.EpochInterval
	currentEpochBytes := make([]byte, 8)
	binary.BigEndian.PutUint64(currentEpochBytes, uint64(currentEpoch))
	cnt := int64(1)
	newEpoch := currentBlockTime / include.EpochInterval

	//如果新周期和当前周期相同（属于同一个周期中）
	if currentEpoch == newEpoch {
		iter := trie.NewIterator(blockCntTrie.NodeIterator(currentEpochBytes))

		//如果当前不是创世周期，从MintCntTrie中读取最后的数量
		if iter.Next() {
			cntBytes := blockCntTrie.Get(append(currentEpochBytes, validator.Bytes()...))
			if cntBytes != nil {
				cnt = int64(binary.BigEndian.Uint64(cntBytes)) + 1
			}
		}
	}

	//更新MintCntTrie
	newCntBytes := make([]byte, 8)
	newEpochBytes := make([]byte, 8)
	binary.BigEndian.PutUint64(newEpochBytes, uint64(newEpoch))
	binary.BigEndian.PutUint64(newCntBytes, uint64(cnt))
	dposContext.BlockCntTrie().TryUpdate(append(newEpochBytes, validator.Bytes()...), newCntBytes)
}