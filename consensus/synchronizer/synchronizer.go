package synchronizer

import (
	"sync"
	"time"

	"github.com/iost-official/go-iost/account"

	"github.com/golang/protobuf/proto"
	"github.com/iost-official/go-iost/common"
	"github.com/iost-official/go-iost/consensus/pob"
	msgpb "github.com/iost-official/go-iost/consensus/synchronizer/pb"
	"github.com/iost-official/go-iost/core/block"
	"github.com/iost-official/go-iost/core/blockcache"
	"github.com/iost-official/go-iost/core/global"
	"github.com/iost-official/go-iost/ilog"
	"github.com/iost-official/go-iost/p2p"
	"github.com/uber-go/atomic"
)

var (
	confirmNumber int64
	// blockHashQueryAdvance+maxBlockHashQueryNumber<=pob.maxBlockNumber
	maxBlockHashQueryNumber int64 = 100
	retryTime                     = 3 * time.Second
	checkTime                     = 3 * time.Second
	syncHeightTime                = 3 * time.Second
	heightAvailableTime     int64 = 22 * 3
	heightTimeout           int64 = 100 * 22 * 3
	continuousNum           int
	syncNumber              int64
	printInterval           int64 = 1000
)

// Synchronizer defines the functions of synchronizer module
type Synchronizer interface {
	Start() error
	Stop()
}

//SyncImpl is the implementation of Synchronizer.
type SyncImpl struct {
	account         *account.KeyPair
	p2pService      p2p.Service
	blockCache      blockcache.BlockCache
	baseVariable    global.BaseVariable
	dc              DownloadController
	heightMap       *sync.Map
	syncEnd         atomic.Int64
	lastPrintHeight atomic.Int64

	messageChan     chan p2p.IncomingMessage
	syncHeightChan  chan p2p.IncomingMessage
	recvBlockChan   chan p2p.IncomingMessage
	pobBlockChan    chan *pob.BlockMessage
	pobResponseChan chan *pob.BlockMessage
	exitSignal      chan struct{}
	wg              *sync.WaitGroup
}

// NewSynchronizer returns a SyncImpl instance.
func NewSynchronizer(account *account.KeyPair, basevariable global.BaseVariable, blkcache blockcache.BlockCache, p2pserv p2p.Service, pbc chan *pob.BlockMessage) (*SyncImpl, error) {
	sy := &SyncImpl{
		account:         account,
		p2pService:      p2pserv,
		blockCache:      blkcache,
		baseVariable:    basevariable,
		heightMap:       new(sync.Map),
		wg:              new(sync.WaitGroup),
		pobBlockChan:    pbc,
		pobResponseChan: make(chan *pob.BlockMessage, cap(pbc)),
	}
	var err error
	sy.dc, err = NewDownloadController(sy.reqSyncBlock)
	if err != nil {
		return nil, err
	}

	sy.messageChan = sy.p2pService.Register("sync message",
		p2p.SyncBlockRequest,
		p2p.SyncBlockHashRequest,
		p2p.SyncBlockHashResponse,
	)

	sy.recvBlockChan = sy.p2pService.Register("consensus channel", p2p.SyncBlockResponse)
	sy.syncHeightChan = sy.p2pService.Register("sync height", p2p.SyncHeight)
	sy.exitSignal = make(chan struct{})

	continuousNum = basevariable.Continuous()
	confirmNumber = int64(len(blkcache.LinkedRoot().Active()))*2/3 + 1
	syncNumber = int64(len(blkcache.LinkedRoot().Active())) * int64(continuousNum)
	ilog.Infof("NewSynchronizer confirmNumber:%v", confirmNumber)
	return sy, nil
}

// Start starts the synchronizer module.
func (sy *SyncImpl) Start() error {
	sy.dc.Start()
	sy.wg.Add(4)

	go sy.syncHeightLoop()
	go sy.messageLoop()
	go sy.initializer()
	go sy.blockLoop()
	return nil
}

// Stop stops the synchronizer module.
func (sy *SyncImpl) Stop() {
	sy.dc.Stop()
	close(sy.exitSignal)
	sy.wg.Wait()
}

func (sy *SyncImpl) initializer() {
	defer sy.wg.Done()
	if sy.baseVariable.Mode() != global.ModeInit {
		return
	}
	for {
		select {
		case <-time.After(retryTime):
			if sy.baseVariable.BlockChain().Length() == 0 {
				ilog.Errorf("block chain is empty")
				return
			}
			sy.baseVariable.SetMode(global.ModeNormal)
			sy.checkSync()
			return
		case <-sy.exitSignal:
			return
		}
	}
}

func (sy *SyncImpl) blockLoop() {
	ilog.Infof("start sync blockloop")
	defer sy.wg.Done()
	for {
		select {
		case incomingMessage, ok := <-sy.recvBlockChan:
			if !ok {
				ilog.Infof("recvBlockChan has closed")
				return
			}
			verifyBufferLen.Set(float64(len(sy.pobBlockChan)), nil)
			responseBlockCount.Add(1, nil)

			var blk block.Block
			err := blk.Decode(incomingMessage.Data())
			if err != nil {
				ilog.Error("fail to decode block")
				continue
			}
			select {
			case sy.pobBlockChan <- &pob.BlockMessage{Blk: &blk, P2PType: incomingMessage.Type(), From: incomingMessage.From(), Ch: sy.pobResponseChan}:
				sy.dc.StopTimeout(string(blk.HeadHash()), incomingMessage.From())
			default:
			}
		case vbm, ok := <-sy.pobResponseChan:
			if !ok {
				ilog.Infof("pobResponseChan has closed")
				return
			}
			responseBufferLen.Set(float64(len(sy.pobResponseChan)), nil)
			sy.dc.FreePeer(string(vbm.Blk.HeadHash()), vbm.From)
		case <-sy.exitSignal:
			return
		}
	}
}

func (sy *SyncImpl) syncHeightLoop() {
	defer sy.wg.Done()
	syncHeightTicker := time.NewTicker(syncHeightTime)
	checkTicker := time.NewTicker(checkTime)
	for {
		select {
		case <-syncHeightTicker.C:
			num := sy.blockCache.Head().Head.Number
			sh := &msgpb.SyncHeight{Height: num, Time: time.Now().Unix()}
			bytes, err := proto.Marshal(sh)
			if err != nil {
				ilog.Errorf("marshal syncheight failed. err=%v", err)
				continue
			}
			ilog.Debugf("broadcast sync height")
			sy.p2pService.Broadcast(bytes, p2p.SyncHeight, p2p.UrgentMessage)
		case req := <-sy.syncHeightChan:
			var sh msgpb.SyncHeight
			err := proto.Unmarshal(req.Data(), &sh)
			if err != nil {
				ilog.Errorf("unmarshal syncheight failed. err=%v", err)
				continue
			}
			if shIF, ok := sy.heightMap.Load(req.From()); ok {
				if shOld, ok := shIF.(*msgpb.SyncHeight); ok {
					if shOld.Height == sh.Height {
						continue
					}
				}
			}
			sy.heightMap.Store(req.From(), &sh)
		case <-checkTicker.C:
			sy.checkSync()
			sy.checkGenBlock()
			sy.checkSyncProcess()
		case <-sy.exitSignal:
			syncHeightTicker.Stop()
			checkTicker.Stop()
			return
		}
	}
}

func (sy *SyncImpl) checkSync() bool {
	if sy.baseVariable.Mode() != global.ModeNormal {
		return false
	}
	heights := make([]int64, 0, 0)
	heights = append(heights, sy.blockCache.Head().Head.Number)
	now := time.Now().Unix()
	sy.heightMap.Range(func(k, v interface{}) bool {
		sh, ok := v.(*msgpb.SyncHeight)
		if !ok || sh.Time+heightAvailableTime < now {
			if sh.Time+heightTimeout < now {
				sy.heightMap.Delete(k)
			}
			return true
		}
		heights = append(heights, 0)
		r := len(heights) - 1
		for 0 < r && heights[r-1] > sh.Height {
			heights[r] = heights[r-1]
			r--
		}
		heights[r] = sh.Height
		return true
	})
	netHeight := heights[len(heights)/2]
	ilog.Debugf("check sync, heights: %+v", heights)
	if netHeight-sy.lastPrintHeight.Load() > printInterval {
		ilog.Infof("sync heights: %+v", heights)
		sy.lastPrintHeight.Store(netHeight)
	}
	if netHeight > sy.blockCache.Head().Head.Number+syncNumber {
		sy.baseVariable.SetMode(global.ModeSync)
		sy.dc.ReStart()
		sy.syncEnd.Store(netHeight)
		go sy.queryBlocksHash(netHeight)
		return true
	}
	return false
}

func (sy *SyncImpl) checkGenBlock() bool {
	if sy.baseVariable.Mode() != global.ModeNormal || sy.blockCache.Head().Head.Number <= sy.syncEnd.Load() {
		return false
	}
	bcn := sy.blockCache.Head()
	witness := sy.account.ReadablePubkey()
	for bcn != nil && bcn.Block.Head.Witness == witness {
		bcn = bcn.GetParent()
	}
	if bcn == nil {
		return false
	}
	height := sy.blockCache.LinkedRoot().Head.Number
	var num int64
	witness = bcn.Block.Head.Witness
	for i := int64(0); i < confirmNumber*int64(continuousNum); i++ {
		if bcn == nil {
			break
		}
		if witness == bcn.Block.Head.Witness {
			num++
		}
		bcn = bcn.GetParent()
	}
	endNumber := sy.blockCache.Head().Head.Number
	if num > int64(continuousNum) && sy.syncEnd.Load() < endNumber {
		ilog.Debugf("num: %v, continuousNum: %v", num, continuousNum)
		startNumber := height + 1
		if sy.syncEnd.Load()+1 > startNumber {
			startNumber = sy.syncEnd.Load() + 1
		}
		if startNumber+maxBlockHashQueryNumber-1 < endNumber {
			endNumber = startNumber + maxBlockHashQueryNumber - 1
		}
		sy.syncEnd.Store(endNumber)
		sy.broadcastBlockHashQuery(&msgpb.BlockHashQuery{ReqType: msgpb.RequireType_GETBLOCKHASHES, Start: startNumber, End: endNumber, Nums: nil})
		return true
	}
	return false
}

func (sy *SyncImpl) broadcastBlockHashQuery(hr *msgpb.BlockHashQuery) {
	bytes, err := proto.Marshal(hr)
	if err != nil {
		ilog.Errorf("marshal blockhashquery failed. err=%v", err)
		return
	}
	ilog.Debugf("[sync] request block hash. reqtype=%v, start=%v, end=%v, nums size=%v", hr.ReqType, hr.Start, hr.End, len(hr.Nums))
	sy.p2pService.Broadcast(bytes, p2p.SyncBlockHashRequest, p2p.UrgentMessage)
}

func (sy *SyncImpl) queryBlocksHash(aimNumber int64) error {
	ilog.Infof("sync Blocks %v, %v", sy.blockCache.LinkedRoot().Head.Number, aimNumber)
	for sy.blockCache.Head().Head.Number < aimNumber {
		startNumber := sy.blockCache.LinkedRoot().Head.Number + 1
		endNumber := startNumber + maxBlockHashQueryNumber - 1
		if endNumber > aimNumber {
			endNumber = aimNumber
		}
		sy.broadcastBlockHashQuery(&msgpb.BlockHashQuery{ReqType: msgpb.RequireType_GETBLOCKHASHES, Start: startNumber, End: endNumber, Nums: nil})

		startNumber = sy.blockCache.Head().Head.Number + 1
		endNumber = startNumber + maxBlockHashQueryNumber - 1
		if endNumber > aimNumber {
			endNumber = aimNumber
		}
		sy.broadcastBlockHashQuery(&msgpb.BlockHashQuery{ReqType: msgpb.RequireType_GETBLOCKHASHES, Start: startNumber, End: endNumber, Nums: nil})

		time.Sleep(retryTime)
	}
	return nil
}

func (sy *SyncImpl) checkSyncProcess() {
	if sy.baseVariable.Mode() != global.ModeSync {
		return
	}
	ilog.Infof("check sync process: now %v, end %v", sy.blockCache.Head().Head.Number, sy.syncEnd.Load())
	if sy.syncEnd.Load() <= sy.blockCache.Head().Head.Number {
		sy.baseVariable.SetMode(global.ModeNormal)
		sy.dc.ReStart()
	}
}

func (sy *SyncImpl) messageLoop() {
	defer sy.wg.Done()
	for {
		select {
		case req := <-sy.messageChan:
			switch req.Type() {
			case p2p.SyncBlockHashRequest:
				var rh msgpb.BlockHashQuery
				err := proto.Unmarshal(req.Data(), &rh)
				if err != nil {
					ilog.Errorf("Unmarshal BlockHashQuery failed:%v", err)
					break
				}
				go sy.handleHashQuery(&rh, req.From())
			case p2p.SyncBlockHashResponse:
				var rh msgpb.BlockHashResponse
				err := proto.Unmarshal(req.Data(), &rh)
				if err != nil {
					ilog.Errorf("Unmarshal BlockHashResponse failed:%v", err)
					break
				}
				go sy.handleHashResp(&rh, req.From())
			case p2p.SyncBlockRequest:
				var rh msgpb.BlockInfo
				err := proto.Unmarshal(req.Data(), &rh)
				if err != nil {
					break
				}
				go sy.handleBlockQuery(&rh, req.From())
			}
		case <-sy.exitSignal:
			return
		}
	}
}

func (sy *SyncImpl) getBlockHashes(start int64, end int64) *msgpb.BlockHashResponse {
	if end < start {
		return &msgpb.BlockHashResponse{
			BlockInfos: make([]*msgpb.BlockInfo, 0, 0),
		}
	}
	if end > start+maxBlockHashQueryNumber-1 {
		end = start + maxBlockHashQueryNumber - 1
	}
	resp := &msgpb.BlockHashResponse{
		BlockInfos: make([]*msgpb.BlockInfo, 0, end-start+1),
	}
	node := sy.blockCache.Head()
	if node != nil && end > node.Head.Number {
		end = node.Head.Number
	}

	for i := end; i >= start; i-- {
		var hash []byte
		var err error

		for node != nil && i < node.Head.Number {
			node = node.GetParent()
		}

		if node != nil {
			hash = node.Block.HeadHash()
		} else {
			hash, err = sy.baseVariable.BlockChain().GetHashByNumber(i)
			if err != nil {
				ilog.Warnf("Get hash by number from db failed. err=%v, number=%v", err, i)
				continue
			}
		}

		blkInfo := msgpb.BlockInfo{
			Number: i,
			Hash:   hash,
		}
		resp.BlockInfos = append(resp.BlockInfos, &blkInfo)
	}
	for i, j := 0, len(resp.BlockInfos)-1; i < j; i, j = i+1, j-1 {
		resp.BlockInfos[i], resp.BlockInfos[j] = resp.BlockInfos[j], resp.BlockInfos[i]
	}
	return resp
}

func (sy *SyncImpl) handleHashQuery(rh *msgpb.BlockHashQuery, peerID p2p.PeerID) {
	if rh.End < rh.Start || rh.Start < 0 {
		return
	}
	var resp *msgpb.BlockHashResponse

	resp = sy.getBlockHashes(rh.Start, rh.End)

	if len(resp.BlockInfos) == 0 {
		return
	}
	bytes, err := proto.Marshal(resp)
	if err != nil {
		ilog.Errorf("Marshal BlockHashResponse failed:struct=%+v, err=%v", resp, err)
		return
	}
	sy.p2pService.SendToPeer(peerID, bytes, p2p.SyncBlockHashResponse, p2p.NormalMessage)
}

func (sy *SyncImpl) handleHashResp(rh *msgpb.BlockHashResponse, peerID p2p.PeerID) {
	ilog.Debugf("receive block hashes: len=%v, peerID=%v", len(rh.BlockInfos), peerID.Pretty())
	for _, blkInfo := range rh.BlockInfos {
		if blkInfo.Number > sy.blockCache.LinkedRoot().Head.Number && blkInfo.Number <= sy.syncEnd.Load() {
			sy.dc.CreateMission(string(blkInfo.Hash), blkInfo.Number, peerID)
		}
	}
}

func (sy *SyncImpl) handleBlockQuery(rh *msgpb.BlockInfo, peerID p2p.PeerID) {
	var blk *block.Block
	blk, err := sy.blockCache.GetBlockByHash(rh.Hash)
	if err != nil {
		blk, err = sy.baseVariable.BlockChain().GetBlockByHash(rh.Hash)
		if err != nil {
			ilog.Warnf("Fail to get block. from=%v, num=%v,hash=%v", peerID.Pretty(), rh.Number, common.Base58Encode(rh.Hash))
			return
		}
	}
	b, err := blk.Encode()
	if err != nil {
		ilog.Errorf("Fail to encode block: %v, err=%v", rh.Number, err)
		return
	}
	sy.p2pService.SendToPeer(peerID, b, p2p.SyncBlockResponse, p2p.NormalMessage)
}

func (sy *SyncImpl) reqSyncBlock(hash string, p interface{}, peerID interface{}) (bool, bool) {
	bn, ok := p.(int64)
	if !ok {
		ilog.Errorf("Assert p to int64 failed. p=%v", p)
		return false, false
	}
	ilog.Debugf("callback try sync block, num:%v, hash:%v", bn, common.Base58Encode([]byte(hash)))
	bHash := []byte(hash)
	if bcn, err := sy.blockCache.Find(bHash); err == nil {
		if bcn.Type == blockcache.Linked {
			ilog.Debugf("callback block linked, num:%v", bn)
			return false, true
		}
		ilog.Debugf("callback block is a single block, num:%v", bn)
		return false, false
	}
	if bn <= sy.blockCache.LinkedRoot().Head.Number {
		ilog.Debugf("callback block confirmed, num:%v", bn)
		return false, true
	}

	if len(sy.pobBlockChan) > cap(sy.pobBlockChan)-int(sy.dc.DownloadCount()) {
		ilog.Debugf("sync block size:%v", len(sy.pobBlockChan))
		return false, false
	}
	bi := &msgpb.BlockInfo{Number: bn, Hash: bHash}
	bytes, err := proto.Marshal(bi)
	if err != nil {
		ilog.Errorf("Marshal request block failed. struct=%+v, err=%v", bi, err)
		return false, false
	}
	pid, ok := peerID.(p2p.PeerID)
	if !ok {
		return false, false
	}
	sy.p2pService.SendToPeer(pid, bytes, p2p.SyncBlockRequest, p2p.UrgentMessage)
	requestBlockCount.Add(1, nil)
	return true, false
}
