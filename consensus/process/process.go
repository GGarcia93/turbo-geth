package process

import (
	"errors"
	"sort"
	"time"

	"github.com/ledgerwatch/turbo-geth/common"
	"github.com/ledgerwatch/turbo-geth/consensus"
	"github.com/ledgerwatch/turbo-geth/core/types"
	"github.com/ledgerwatch/turbo-geth/log"
	"github.com/ledgerwatch/turbo-geth/params"
)

type Consensus struct {
	Server         consensus.Verifier
	*consensus.API // remote Engine
}

const ttl = time.Minute

var (
	errEmptyHeader  = errors.New("an empty header")
	errNothingToAsk = errors.New("nothing to ask")
)

func NewConsensusProcess(v consensus.Verifier, config *params.ChainConfig, exit chan struct{}) *Consensus {
	c := &Consensus{
		Server: v,
		API:    consensus.NewAPI(config),
	}

	// event loop
	go func() {
	eventLoop:
		for {
			select {
			case req := <-c.API.VerifyHeaderRequests:
				if len(req.Headers) == 0 {
					c.API.VerifyHeaderResponses <- consensus.VerifyHeaderResponse{req.ID, common.Hash{}, errEmptyHeader}
					continue
				}

				if req.Deadline == nil {
					t := time.Now().Add(ttl)
					req.Deadline = &t
				}

				// copy slices and sort. had a data race with downloader
				reqHeaders := make([]reqHeader, len(req.Headers))
				for i := range req.Headers {
					reqHeaders[i] = reqHeader{req.Headers[i], req.Seal[i]}
				}

				sort.Slice(reqHeaders, func(i, j int) bool {
					return reqHeaders[i].header.Number.Cmp(reqHeaders[j].header.Number) == -1
				})

				req.Headers = make([]*types.Header, len(reqHeaders))
				req.Seal = make([]bool, len(reqHeaders))
				for i := range reqHeaders {
					req.Headers[i] = reqHeaders[i].header
					req.Seal[i] = reqHeaders[i].seal
				}

				ancestorsReqs := make([]consensus.HeadersRequest, 0, len(req.Headers))

				for i, header := range req.Headers {
					if header == nil {
						c.API.VerifyHeaderResponses <- consensus.VerifyHeaderResponse{req.ID, common.Hash{}, errEmptyHeader}
						continue eventLoop
					}

					// Short circuit if the header is known
					if h := c.API.GetCachedHeader(header.Hash(), header.Number.Uint64()); h != nil {
						c.API.VerifyHeaderResponses <- consensus.VerifyHeaderResponse{req.ID, header.Hash(), nil}
						continue
					}

					knownParentsSlice, parentsToValidate, ancestorsReq := c.requestParentHeaders(req.ID, header, req.Headers)
					if ancestorsReq != nil {
						ancestorsReqs = append(ancestorsReqs, *ancestorsReq)
					}

					err := c.verifyByRequest(req.ID, header, req.Seal[i], parentsToValidate, knownParentsSlice)
					if errors.Is(err, errNotAllParents) {
						c.addVerifyHeaderRequest(req.ID, header, req.Seal[i], req.Deadline, knownParentsSlice, parentsToValidate)
					}
				}

				ancestorsReq, err := sumHeadersRequestsInRange(req.ID, req.Headers[0].Number.Uint64(), ancestorsReqs...)
				if err != nil {
					log.Error("can't request header ancestors", "reqID", req.ID, "number", req.Headers[0].Number.Uint64(), "err", err)
					continue
				}

				c.API.HeadersRequests <- ancestorsReq

			case parentResp := <-c.API.HeaderResponses:
				if parentResp.Err != nil {
					c.API.VerifyHeaderResponses <- consensus.VerifyHeaderResponse{parentResp.ID, parentResp.Hash, parentResp.Err}

					c.API.ProcessingRequestsMu.Lock()
					delete(c.API.ProcessingRequests, parentResp.ID)
					c.API.ProcessingRequestsMu.Unlock()

					continue
				}

				c.VerifyRequestsCommonAncestor(parentResp.ID, parentResp.Headers)

			// cleanup by timeout
			case <-c.API.CleanupTicker.C:
				c.cleanup()

			case <-exit:
				return
			}
		}
	}()

	// cleanup loop
	go func() {
		for {
			select {
			case req := <-c.API.CleanupCh:
				c.cleanupRequest(req.ReqID, req.BlockNumber)
			case <-exit:
				return
			}
		}
	}()

	return c
}

type reqHeader struct {
	header *types.Header
	seal   bool
}

func (c *Consensus) cleanup() {
	now := time.Now()

	c.API.ProcessingRequestsMu.Lock()

	for reqID, reqBlocks := range c.API.ProcessingRequests {
		for _, req := range reqBlocks {
			if req.Deadline.Before(now) {
				c.API.VerifyHeaderResponses <- consensus.VerifyHeaderResponse{reqID, req.Header.Hash(), errors.New("timeout")}

				delete(c.API.ProcessingRequests, reqID)
			}
		}
	}

	c.API.ProcessingRequestsMu.Unlock()
}

func (c *Consensus) VerifyRequestsCommonAncestor(reqID uint64, headers []*types.Header) {
	if len(headers) == 0 {
		return
	}

	c.API.ProcessingRequestsMu.Lock()
	reqHeaders, ok := c.API.ProcessingRequests[reqID]
	if !ok {
		c.API.ProcessingRequestsMu.Unlock()
		return
	}

	nums := make([]uint64, 0, len(reqHeaders))
	for num := range reqHeaders {
		nums = append(nums, num)
	}
	c.API.ProcessingRequestsMu.Unlock()

	sort.Slice(nums, func(i, j int) bool {
		return nums[i] < nums[j]
	})

	for _, header := range headers {
		c.API.CacheHeader(header)
	}

	knownByRequests := make(map[uint64]map[common.Hash]map[uint64]struct{}) // reqID -> parenthash -> blockToValidate

	for _, num := range nums {
		c.API.ProcessingRequestsMu.Lock()
		req := reqHeaders[num]
		c.API.ProcessingRequestsMu.Unlock()

		appendAncestors(req, headers, knownByRequests)

		err := c.verifyByRequest(req.ID, req.Header, req.Seal, req.ParentsExpected, req.KnownParents)
		if err == nil {
			headers = append(headers, req.Header)
		}
	}
}

func (c *Consensus) verifyByRequest(reqID uint64, header *types.Header, seal bool, parentsExpected int, knownParents []*types.Header) error {
	if len(knownParents) != parentsExpected {
		return errNotAllParents
	}

	err := c.Server.Verify(c.API.Chain, header, knownParents, false, seal)
	if err == nil {
		c.API.CacheHeader(header)
	}

	c.API.VerifyHeaderResponses <- consensus.VerifyHeaderResponse{reqID, header.Hash(), err}

	// remove finished request
	finishedRequest := consensus.FinishedRequest{reqID, header.Number.Uint64()}
	select {
	case c.CleanupCh <- finishedRequest:
	default:
		c.cleanupRequest(finishedRequest.ReqID, finishedRequest.BlockNumber)
	}

	return nil
}

// remove finished request
func (c *Consensus) cleanupRequest(reqID uint64, number uint64) {
	c.API.ProcessingRequestsMu.Lock()
	reqBlocks, ok := c.API.ProcessingRequests[reqID]
	if ok {
		delete(reqBlocks, number)
		if len(reqBlocks) == 0 {
			delete(c.API.ProcessingRequests, reqID)
		}
	}
	c.API.ProcessingRequestsMu.Unlock()
}

func toVerifyRequest(reqID uint64, header *types.Header, seal bool, deadline *time.Time, knownParents []*types.Header, parentsToValidate int) *consensus.VerifyRequest {
	return &consensus.VerifyRequest{
		reqID,
		header,
		seal,
		deadline,
		knownParents,
		parentsToValidate,
		header.Number.Uint64() - uint64(parentsToValidate),
		header.Number.Uint64() - uint64(len(knownParents)) - 1,
	}
}

func (c *Consensus) addVerifyHeaderRequest(reqID uint64, header *types.Header, seal bool, deadline *time.Time, knownParentsSlice []*types.Header, parentsToValidate int) {
	request := toVerifyRequest(reqID, header, seal, deadline, knownParentsSlice, parentsToValidate)

	c.API.ProcessingRequestsMu.Lock()
	blocks, ok := c.API.ProcessingRequests[reqID]
	if !ok {
		blocks = make(map[uint64]*consensus.VerifyRequest)
	}

	blocks[header.Number.Uint64()] = request
	c.API.ProcessingRequests[reqID] = blocks
	c.API.ProcessingRequestsMu.Unlock()
}

func appendAncestors(request *consensus.VerifyRequest, ancestors []*types.Header, knownByRequests map[uint64]map[common.Hash]map[uint64]struct{}) {
	blockNumber := request.Header.Number.Uint64()

	ancestorsMap, ok := knownByRequests[request.ID]
	if !ok {
		ancestorsMap = make(map[common.Hash]map[uint64]struct{}, len(request.KnownParents)+len(ancestors))
		for _, p := range request.KnownParents {
			ancestorsMap[p.Hash()] = map[uint64]struct{}{
				blockNumber: {},
			}
		}
		knownByRequests[request.ID] = ancestorsMap
	}

	for _, parent := range ancestors {
		if parent.Number.Uint64() >= request.From && parent.Number.Uint64() <= request.To {
			parentMap, has := ancestorsMap[parent.Hash()]
			if !has {
				ancestorsMap[parent.Hash()] = map[uint64]struct{}{
					blockNumber: {},
				}

				request.KnownParents = append(request.KnownParents, parent)
			} else {
				_, has = parentMap[blockNumber]
				if !has {
					ancestorsMap[parent.Hash()] = map[uint64]struct{}{
						blockNumber: {},
					}
					request.KnownParents = append(request.KnownParents, parent)
				}
			}
		}
	}
}

func (c *Consensus) HeaderVerification() chan<- consensus.VerifyHeaderRequest {
	return c.API.VerifyHeaderRequests
}

func (c *Consensus) requestParentHeaders(reqID uint64, header *types.Header, reqHeaders []*types.Header) ([]*types.Header, int, *consensus.HeadersRequest) {
	parentsToValidate := c.Server.NeededForVerification(header)
	if parentsToValidate == 0 {
		return nil, 0, nil
	}

	headerNumber := header.Number.Uint64()
	headerParentHash := header.ParentHash

	from := reqHeaders[0].Number.Uint64()
	to := reqHeaders[len(reqHeaders)-1].Number.Uint64()

	parentsToAsk := parentsToValidate

	// don't ask for already requested for verification blocks
	if header.Number.Uint64() > from && header.Number.Uint64() <= to {
		if header.Number.Uint64() >= from+uint64(parentsToValidate) {
			// we're inside the requested range
			parentsToAsk = 0
		} else {
			parentsToAsk = int(int64(from) - (header.Number.Int64() - int64(parentsToAsk)))
		}
	}

	if parentsToAsk > 0 {
		headerNumber = from - 1
		headerParentHash = reqHeaders[0].ParentHash
	}

	knownParents, ancestorsReq := c.requestHeadersNotFromRange(reqID, headerNumber, headerParentHash, uint64(parentsToAsk))
	knownParentsFromRange := c.checkHeadersFromRange(header, reqHeaders, uint64(parentsToAsk), uint64(parentsToValidate))

	knownParents = append(knownParents, knownParentsFromRange...)

	return knownParents, parentsToValidate, &ancestorsReq
}

var errNotAllParents = errors.New("not all parents are gathered")

func (c *Consensus) requestHeadersNotFromRange(reqID uint64, highestBlock uint64, highestKnown common.Hash, parentsToGet uint64) ([]*types.Header, consensus.HeadersRequest) {
	highestParentHash := highestKnown
	highestParentNumber := highestBlock

	var minHeader uint64
	if highestBlock > parentsToGet-1 {
		minHeader = highestBlock - parentsToGet + 1
	}

	known := make([]*types.Header, 0, highestBlock-minHeader)

	for parentBlockNum := highestBlock; parentBlockNum >= minHeader; parentBlockNum-- {
		parentBlock := c.API.GetCachedHeader(highestKnown, parentBlockNum)
		if parentBlock == nil {
			break
		}

		highestKnown = parentBlock.ParentHash

		known = append(known, parentBlock)

		if highestParentNumber < parentBlock.Number.Uint64() {
			highestParentNumber = parentBlock.Number.Uint64()
			highestParentHash = parentBlock.Hash()
		}
	}

	return known, consensus.HeadersRequest{
		reqID,
		highestParentHash,
		highestParentNumber,
		parentsToGet - uint64(len(known)),
	}
}

func sumHeadersRequestsInRange(reqID uint64, from uint64, reqs ...consensus.HeadersRequest) (consensus.HeadersRequest, error) {
	if len(reqs) == 0 {
		return consensus.HeadersRequest{}, errNothingToAsk
	}

	maxBlockNumber := reqs[0].HighestBlockNumber
	maxBlockHash := reqs[0].HighestHash
	minBlockToGet := maxBlockNumber - reqs[0].Number + 1

	for _, req := range reqs {
		if req.ID != reqID {
			continue
		}

		if req.Number == 0 {
			continue
		}

		if req.HighestBlockNumber > maxBlockNumber && req.HighestBlockNumber < from {
			maxBlockNumber = req.HighestBlockNumber
			maxBlockHash = req.HighestHash
		}

		if req.HighestBlockNumber-req.Number+1 < minBlockToGet {
			minBlockToGet = req.HighestBlockNumber - req.Number + 1
		}
	}

	return consensus.HeadersRequest{
		reqID,
		maxBlockHash,
		maxBlockNumber,
		maxBlockNumber - minBlockToGet + 1,
	}, nil
}

func (c *Consensus) checkHeadersFromRange(highestHeader *types.Header, requestedHeaders []*types.Header, parentsToGet, parentsToValidate uint64) []*types.Header {
	parentsToGet = parentsToValidate - parentsToGet
	if parentsToGet <= 0 {
		return nil
	}

	idx := -1
	for i, h := range requestedHeaders {
		if h.Number.Uint64() == highestHeader.Number.Uint64() {
			idx = i
			break
		}
	}
	if idx < 0 {
		return nil
	}

	if idx-int(parentsToGet) < 0 {
		return nil
	}

	return requestedHeaders[idx-int(parentsToGet) : idx]
}

func (c *Consensus) VerifyResults() <-chan consensus.VerifyHeaderResponse {
	return c.API.VerifyHeaderResponses
}