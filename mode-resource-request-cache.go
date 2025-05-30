package web3protocol

import (
	"sync"
	"time"
	"net/url"
	"strings"
	"context"
	"math/big"
	"regexp"
	// "errors"
	// "fmt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/sirupsen/logrus"
)

// Contains the caching infos of all the resources requested by the client that implements
// ERC-7774 (includes "evm-events" in the Cache-Control header)
type ResourceRequestCachingTracker struct {
	// Per-chain caching trackers
	ChainCachingTrackers map[int]*ResourceRequestChainCachingTracker

	// Pointer to the Client, to access the config and make requests
	Client *Client

	// Mutex to protect the struct
	Mutex sync.RWMutex
}

func NewResourceRequestCachingTracker(client *Client) ResourceRequestCachingTracker {
	return ResourceRequestCachingTracker{
		ChainCachingTrackers: make(map[int]*ResourceRequestChainCachingTracker),
		Client: client,
	}
}

func (c *ResourceRequestCachingTracker) GetOrCreateChainCachingTracker(chainId int) (chainCachingTracker *ResourceRequestChainCachingTracker) {
	c.Mutex.Lock()
	defer c.Mutex.Unlock()

	chainCachingTracker, ok := c.ChainCachingTrackers[chainId]
	if !ok {
		chainCachingTracker = &ResourceRequestChainCachingTracker{
			ChainId:              chainId,
			ResourcesCachingInfos: make(map[common.Address]map[string]*ResourceCachingInfos),
			EventsCheckWorkerStopChan: make(chan bool),
			GlobalCachingTracker: c,
		}
		c.ChainCachingTrackers[chainId] = chainCachingTracker

		chainCachingTracker.Activate()

		// Start the worker that checks for events to be processed
		go chainCachingTracker.checkEventsWorker(12 * time.Second)
	}

	return
}

func (c *ResourceRequestCachingTracker) GetChainCachingTracker(chainId int) (chainCachingTracker *ResourceRequestChainCachingTracker, ok bool) {
	c.Mutex.RLock()
	defer c.Mutex.RUnlock()

	chainCachingTracker, ok = c.ChainCachingTrackers[chainId]
	return
}

// Like ResourceRequestCachingTracker, but for a specific chain
type ResourceRequestChainCachingTracker struct {
	// The chain ID of this tracker
	ChainId int
	// The last block number we have processed
	LastBlockNumber uint64
	// The time of the last successfull event check
	LastEventsCheck time.Time
	// Is it active? It is not active if the event check worker has trouble and is
	// not able to track events
	IsActive bool

	// The cache of indifidual resources requested by the client
	// map[contractAddress][pathQuery]
	ResourcesCachingInfos map[common.Address]map[string]*ResourceCachingInfos
	// Last read date of the cache
	LastRead time.Time

	// There is a goroutine worker that checks for events to be processed
	// This is the channel to stop it
	EventsCheckWorkerStopChan chan bool

	// Mutex to protect access to the struct
	Mutex sync.RWMutex

	// Pointer to the global caching tracker
	GlobalCachingTracker *ResourceRequestCachingTracker
}

type AddressPathQuery struct {
	ContractAddress common.Address
	PathQuery string
}

// Caching infos for a specific resource
type ResourceCachingInfos struct {
	// The last modified date of the resource
	LastModified time.Time

	// The ETag of the resource
	ETag string

	// Others contract can proxy paths of this contract, and they can indicate with 
	// '"Cache-Control: evem-events="<contractAddress><pathQuery>"'
	// to listen for cache clearing events for this contract
	// So here we store the list of external contracts paths that proxy this resource
	CacheClearEventListeners []AddressPathQuery
}

// The worker that checks for events to be processed had issues and is not able to track events,
// so we mark the tracker as inactive, and clear cache
func (c* ResourceRequestChainCachingTracker) Desactivate() {
	c.Mutex.Lock()
	defer c.Mutex.Unlock()

	if !c.IsActive {
		return
	}

	c.IsActive = false

	// Clear the cache
	c.ResourcesCachingInfos = make(map[common.Address]map[string]*ResourceCachingInfos)

	c.GlobalCachingTracker.Client.Logger.WithFields(logrus.Fields{
		"domain": "resourceRequestModeCaching",
		"chain": c.ChainId,
	}).Info("Cache tracking desactivated, cache cleared.")
}

// The worker that checks for events to be processed is now able to track events
func (c* ResourceRequestChainCachingTracker) Activate() {
	c.Mutex.Lock()
	defer c.Mutex.Unlock()

	c.ActivateUnsafe()
}

// Activate() without the mutex
func (c* ResourceRequestChainCachingTracker) ActivateUnsafe() {
	if c.IsActive {
		return
	}

	c.LastBlockNumber = 0
	c.LastEventsCheck = time.Now()
	c.IsActive = true
	c.LastRead = time.Now()


	c.GlobalCachingTracker.Client.Logger.WithFields(logrus.Fields{
		"domain": "resourceRequestModeCaching",
		"chain": c.ChainId,
	}).Info("Cache tracking activated.")
}

func (c *ResourceRequestChainCachingTracker) GetResourceCachingInfos(contractAddress common.Address, pathQuery string) (resourceCachingInfos *ResourceCachingInfos, ok bool) {
	c.Mutex.Lock()
	defer c.Mutex.Unlock()

	// If the caching tracker is not active, return nil
	if !c.IsActive {
		return
	}

	contractResources, ok := c.ResourcesCachingInfos[contractAddress]
	if !ok {
		return
	}

	resourceCachingInfos, ok = contractResources[pathQuery]

	// Log the cache hit
	if ok {
		c.LastRead = time.Now()
	}

	return
}

func (c *ResourceRequestChainCachingTracker) GetResourceCachingInfosByPattern(contractAddress common.Address, pathQueryPattern string) (resourceCachingInfos map[string]*ResourceCachingInfos, err error) {
	c.Mutex.Lock()
	defer c.Mutex.Unlock()

	// If the caching tracker is not active, return nil
	if !c.IsActive {
		return
	}

	contractResources, ok := c.ResourcesCachingInfos[contractAddress]
	if !ok {
		return
	}

	// Special case: a single "*" will return everything
	resourceCachingInfos = make(map[string]*ResourceCachingInfos)
	if pathQueryPattern == "*" {
		for pathQuery, cachingInfos := range contractResources {
			resourceCachingInfos[pathQuery] = cachingInfos
		}
	// General case: 
	// A path can have wildcards in path components
	// e.g. 
	// "/path/*/to/*" will match "/path/1/to/2" and "/path/3/to/4", but 
	//   not "/path/1/to", "/path/1/to/", "/path/1/to/2/3"
	// "/*" will match "/path" but not "/", "/path/1"
	} else {
		for pathQuery, cachingInfos := range contractResources {
			pathQueryParts := strings.Split(pathQuery, "/")
			pathQueryPatternParts := strings.Split(pathQueryPattern, "/")
			if len(pathQueryParts) != len(pathQueryPatternParts) {
				continue
			}
			match := true
			for i, pathQueryPart := range pathQueryParts {
				// Not a wildcard : It must match exactly
				if pathQueryPatternParts[i] != "*" && pathQueryPart != pathQueryPatternParts[i] {
					match = false
					break
				}
				// Wildcard : It can match anything, but not empty
				if pathQueryPatternParts[i] == "*" && pathQueryPart == "" {
					match = false
					break
				}
			}
			if match {
				resourceCachingInfos[pathQuery] = cachingInfos
			}
		}
	}

	// Log the cache hit
	if len(resourceCachingInfos) > 0 {
		c.LastRead = time.Now()
	}

	return
}

func (c *ResourceRequestChainCachingTracker) SetResourceCachingInfos(contractAddress common.Address, pathQuery string, resourceCachingInfos *ResourceCachingInfos) {
	c.Mutex.Lock()
	defer c.Mutex.Unlock()

	// If the caching tracker is not active, activate it
	if !c.IsActive {
		c.ActivateUnsafe()
	}

	contractResources, ok := c.ResourcesCachingInfos[contractAddress]
	if !ok {
		contractResources = make(map[string]*ResourceCachingInfos)
		c.ResourcesCachingInfos[contractAddress] = contractResources
	}

	contractResources[pathQuery] = resourceCachingInfos
}

// Delete the caching infos by pathQuery. Support wildcard pathQuery.
func (c *ResourceRequestChainCachingTracker) DeleteResourceCachingInfos(contractAddress common.Address, pathQuery string) {
	c.Mutex.Lock()
	defer c.Mutex.Unlock()

	contractResourcesCachingInfos, ok := c.ResourcesCachingInfos[contractAddress]
	if !ok {
		return
	}

	// Special case : a single "*" will delete everything
	if pathQuery == "*" {
		delete(c.ResourcesCachingInfos, contractAddress)

		c.GlobalCachingTracker.Client.Logger.WithFields(logrus.Fields{
			"domain": "resourceRequestModeCaching",
			"chain": c.ChainId,
			"contractAddress": contractAddress,
		}).Info("Cache infos cleared for all paths.")

	// Otherwise: Single path matching
	} else {

		pathCachingInfos, ok := contractResourcesCachingInfos[pathQuery]
		if !ok {
			return
		}

		delete(contractResourcesCachingInfos, pathQuery)

		c.GlobalCachingTracker.Client.Logger.WithFields(logrus.Fields{
			"domain": "resourceRequestModeCaching",
			"chain": c.ChainId,
			"contractAddress": contractAddress,
			"etag": pathCachingInfos.ETag,
		}).Info("Cache infos cleared for path ", pathQuery)
	}
}


// Goroutine worker that checks for events to be processed
func (cct *ResourceRequestChainCachingTracker) checkEventsWorker(eventsCheckInterval time.Duration) {
	ticker := time.NewTicker(eventsCheckInterval)
	defer ticker.Stop()

	log := cct.GlobalCachingTracker.Client.Logger

	var eventCheckingMutex sync.Mutex
	eventCheckingIsRunning := false
	var systemRpc string

	// Func to get the logFields for the logging
	logFields := func(extraFields logrus.Fields) logrus.Fields {
		fields := logrus.Fields{
			"domain": "resourceRequestModeCaching",
			"chain": cct.ChainId,
			"lastBlockNumber": cct.LastBlockNumber,
			"worker": "checkEvents",
			"rpc": systemRpc,
		}
		if extraFields != nil {
			for k, v := range extraFields {
				fields[k] = v
			}
		}
		return fields
	}	

	// Preparing the ethClient
	systemRpc, err := cct.GlobalCachingTracker.Client.GetSystemRpcUrl(cct.ChainId)
	if err != nil {
		log.WithFields(logFields(nil)).Error("Could not find a RPC, exiting.")
		return
	}
	ethClient, err := ethclient.Dial(systemRpc)
	if err != nil {
		log.WithFields(logFields(nil)).Error("Could not connect to chain, exiting.")
		return
	}
	defer ethClient.Close()

	// Log the start
	log.WithFields(logFields(nil)).Info("Worker started.")


	// The event check task
	eventCheckTask := func() {
		// Check if action is already running
		eventCheckingMutex.Lock()
		if eventCheckingIsRunning {
			log.WithFields(logFields(nil)).Debug("Check skipped because another is still running.")
			eventCheckingMutex.Unlock()
			return
		}
		eventCheckingIsRunning = true
		eventCheckingMutex.Unlock()
		defer func() {
			eventCheckingMutex.Lock()
			eventCheckingIsRunning = false
			eventCheckingMutex.Unlock()
		}()

		// If the chain caching tracker is not active, we skip
		if !cct.IsActive {
			return
		}

		// If the last time we read the cache was more than XX minutes ago, we desactivate the tracker
		// Reason : tracking use one RPC call every XX seconds, and if the cache is not used enough,
		// then you end up using more RPC calls than you save
		if time.Since(cct.LastRead) > 10 * time.Minute {
			log.WithFields(logFields(nil)).Info("Cache not used for a long time, desactivating the tracker.")
			cct.Desactivate()
			return
		}

		// We allow a random failure of the check events worker (due to block number fail or event fetching
		// fail), but if it fails too often, we desactivate the tracker
		// If failed for more than 1 minute, we desactivate the tracker
		if time.Since(cct.LastEventsCheck) > 1 * time.Minute {
			log.WithFields(logFields(nil)).Error("Check events failed for more than 1 minute, desactivating the tracker.")
			cct.Desactivate()
			return
		}

		// Get the current block number
		currentBlockNumber, err := ethClient.BlockNumber(context.Background())
		if err != nil {
			log.WithFields(logFields(nil)).Error("Could not get the current block number, skipping check")
			return
		}

		// Get the logs
		fromBlockNumber := new(big.Int).SetUint64(currentBlockNumber)
		if cct.LastBlockNumber > 0 {
			fromBlockNumber = new(big.Int).SetUint64(cct.LastBlockNumber + 1)
		}
		logs, err := ethClient.FilterLogs(context.Background(), ethereum.FilterQuery{
			FromBlock: fromBlockNumber,
			ToBlock:   nil,
			Addresses: []common.Address{},
			// keccak256("ClearPathCache(string[])"):
			// 0xc38a9b9ff90edb266ea753dddfda98041dac078259df7188da47699190a28219
			Topics:    [][]common.Hash{{common.HexToHash("0xc38a9b9ff90edb266ea753dddfda98041dac078259df7188da47699190a28219")}},
		})
		if err != nil {
			log.WithFields(logFields(nil)).Error("Could not get the logs, skipping check")
			return
		}
		log.WithFields(logFields(nil)).Debug("ClearPathCache logs fetched: ", len(logs))

		// Process the logs
		for _, logEntry := range logs {
			// Get the pathQueries to be cleared: logEntry.Data is an ABI-encoded array of strings
			stringArrayType, _ := abi.NewType("string[]", "", nil)
			abiArguments := abi.Arguments{
				{Type: stringArrayType},
			}
			unpackedValues, err := abiArguments.UnpackValues(logEntry.Data)
			if err != nil {
				log.WithFields(logFields(nil)).Error("Could not unpack the log data, skipping...")
				continue
			}
			pathQueries := unpackedValues[0].([]string)

			log.WithFields(logFields(logrus.Fields{
				"contractAddress": logEntry.Address,
			})).Info("Cache clear requested for paths ", pathQueries)
			
			// Delete the caching infos for each pathQuery
			for _, pathQuery := range pathQueries {
				resourcesToClear := make(map[string]*ResourceCachingInfos)
				// Find the resources to clear (handles wildcards)
				_resourcesToClear, err := cct.GetResourceCachingInfosByPattern(logEntry.Address, pathQuery)
				if err != nil {
					log.WithFields(logFields(nil)).Error("Could not get the resources to clear, skipping...")
					continue
				}
				resourcesToClear = _resourcesToClear

				// For each resource to clear, delete the cache
				for pathQuery, resourceCachingInfos := range resourcesToClear {
					// Delete the cache for the pathQuery
					cct.DeleteResourceCachingInfos(logEntry.Address, pathQuery)

					// If the resource has listeners for cache clear events, clear them too
					for _, proxiedCacheClearLocation := range resourceCachingInfos.CacheClearEventListeners {
						cct.DeleteResourceCachingInfos(proxiedCacheClearLocation.ContractAddress, proxiedCacheClearLocation.PathQuery)
					}
				}
			}

			// We may have got a log more recent than the current block number that we fetched earlier,
			// so update the last block number
			if logEntry.BlockNumber > currentBlockNumber {
				currentBlockNumber = logEntry.BlockNumber
			}
		}

		// Update the last block number
		cct.LastBlockNumber = currentBlockNumber

		// Update the last event check time
		cct.LastEventsCheck = time.Now()
	}


	// Main loop
	// Execute the event check task right away
	go eventCheckTask()
	// Then it's the loop
	for {
		select {
		case <- ticker.C:
			go eventCheckTask()

		case <- cct.EventsCheckWorkerStopChan:
			// Stop signal received, exit goroutine
			log.WithFields(logFields(nil)).Info("Stopping the worker.")
			return
		}
	}

}



// Serialize the arguments of a resource request method, for the purpose of making a cache key
func SerializeResourceRequestMethodArgValues(argValues []interface{}) (pathQuery string) {
	pathQuery = ""
	pathnameParts := argValues[0].([]string)
	if len(pathnameParts) == 0 {
		pathQuery = "/"
	} else {
		for _, pathnamePart := range pathnameParts {
			pathQuery += "/" + url.PathEscape(pathnamePart)
		}
	}

	// Extract the query
	params := argValues[1].([]struct {
		Key   string
		Value string
	})
	if len(params) > 0 {
		pathQuery += "?"
		for _, param := range params {
			pathQuery += "&" + url.QueryEscape(param.Key) + "=" + url.QueryEscape(param.Value)
		}
	}

	return
}

// Parse the Cache-Control header value into a map of directives
func GetCacheControlHeaderDirectives(headerValue string) (directives map[string]string) {
	directives = make(map[string]string)
	
	// Regular expression to match key-value pairs in Cache-Control header
	// Matches three patterns:
	// 1. key="quoted value" (with quoted values that can contain spaces)
	// 2. key=value (unquoted values without spaces)
	// 3. standalone keys (flags without values)
	re := regexp.MustCompile(`(?:^|[,;]\s*)([^=,;\s]+)(?:\s*=\s*(?:"((?:[^"\\]|\\.)*)"|([^,;\s]*)))?`)
	
	matches := re.FindAllStringSubmatch(headerValue, -1)
	for _, match := range matches {
		key := match[1]
		var value string
		
		// If we have a quoted value (match[2]), use that; otherwise use the unquoted value (match[3])
		if match[2] != "" {
			value = match[2]
			// Proper unescaping of quoted-string according to 
			// https://www.rfc-editor.org/rfc/rfc9110.html#section-5.6.4
			// to fulfill cache-control directive requirements from
			// https://www.rfc-editor.org/rfc/rfc9111#section-5.2
			// would need to be implemented; for now we'll only unescape quotes
			value = strings.ReplaceAll(value, "\\\"", "\"")
		} else if match[3] != "" {
			value = match[3]			
		}
		
		directives[key] = value
	}
	
	return
}
