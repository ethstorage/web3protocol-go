package web3protocol

import (
	"time"
	"sync"
	// "fmt"
	"io"
	"context"

	"github.com/sirupsen/logrus"
	"github.com/ethereum/go-ethereum/ethclient"
	golanglru2 "github.com/hashicorp/golang-lru/v2/expirable"
)

type Client struct {
	Config *Config
	Logger *logrus.Logger

	// The list of RPCs per chain. They are filled with the content of the config,
	// and contains their state (available, too many requests, unauthorized)
	Rpcs map[int][]*Rpc
	RpcsMutex sync.RWMutex
	
	// The request queue, and their channel to notify the caller with the response
	// Used to aggregate requests to the same URL
	RequestQueue map[RequestQueueKey][]chan *RequestQueueResponse
	RequestQueueMutex sync.Mutex
	// One worker per request, limited to X workers
	// RequestWorkerSemaphone chan struct{}

	// Cache for domain name resolution
	DomainNameResolutionCache *localCache

	// Cache for resolve mode determination
	ResolveModeCache *golanglru2.LRU[ResolveModeCacheKey, ResolveMode]

	// ERC-7774: Resource request mode : Cache invalidation tracking
	ResourceRequestCachingTracker ResourceRequestCachingTracker
}

// Requests are grouped by URL and some specific request headers
type RequestQueueKey struct {
	// The URL of the request
	URL string
	// Some specific request headers, which may alter the response
	HttpHeaderIfNoneMatch string
	HttpHeaderIfModifiedSince string
	HttpHeaderAcceptEncoding string
}

// The response to a request queue entry
type RequestQueueResponse struct {
	// The response itself (includes plenty of information of the request processing)
	fetchedUrl *FetchedWeb3URL
	// The error
	// Should usually be of Web3ProtocolError type, which include the HTTP status code to return to 
	// the client, and additional information about the issue. But it can be a generic error.
	err error
}

// A RPC, containing its URL and state
type Rpc struct {
	// The RPC config
	Config ChainRPCConfig

	// The state of the RPC
	State RpcState

	// We authorize X parralel requests to the RPC
	RequestSemaphone chan struct{}
}

type RpcState string
const (
	RpcStateAvailable RpcState = "available"
	RpcStateTooManyRequests RpcState = "tooManyRequests"
	RpcStateUnauthorized RpcState = "unauthorized"
)


/**
 * You'll need to instantiate a client to make calls.
 */
func NewClient(config *Config) (client *Client) {
	client = &Client{
		Config: config,
		Logger: logrus.New(),

		RequestQueue: make(map[RequestQueueKey][]chan *RequestQueueResponse),

		// RequestWorkerSemaphone: make(chan struct{}, 5),

		DomainNameResolutionCache: newLocalCache(time.Duration(config.NameAddrCacheDurationInMinutes)*time.Minute, 10*time.Minute),
		ResolveModeCache: golanglru2.NewLRU[ResolveModeCacheKey, ResolveMode](1000, nil, time.Duration(0)),
	}
	client.ResourceRequestCachingTracker = NewResourceRequestCachingTracker(client)

	// Initialize the RPCs
	client.Rpcs = make(map[int][]*Rpc)
	for chainId, chainConfig := range config.Chains {
		client.Rpcs[chainId] = make([]*Rpc, 0)
		// Max number of concurrent requests : default to 5
		maxNumberOfConcurrentRequests := chainConfig.RPC.MaxConcurrentRequests
		if maxNumberOfConcurrentRequests == 0 {
			maxNumberOfConcurrentRequests = 5
		}
		client.Rpcs[chainId] = append(client.Rpcs[chainId], &Rpc{
				Config: chainConfig.RPC,
				State: RpcStateAvailable,
				RequestSemaphone: make(chan struct{}, maxNumberOfConcurrentRequests),
			})
	}

	return
}

/**
 * The main function of the package.
 * For a given full web3:// url ("web3://xxxx"), returns a structure containing
 * the bytes output and the HTTP code and headers, as well as plenty of informations on
 * how the processing was done.
 */
func (client *Client) FetchUrl(url string, httpHeaders map[string]string) (fetchedUrl *FetchedWeb3URL, err error) {
	// Prepare the request queue key
	requestQueueKey := RequestQueueKey{
		URL: url,
		HttpHeaderIfNoneMatch: httpHeaders["If-None-Match"],
		HttpHeaderIfModifiedSince: httpHeaders["If-Modified-Since"],
		HttpHeaderAcceptEncoding: httpHeaders["Accept-Encoding"],
	}

	// Prepare the request queue response channel
	requestQueueResponseChannel := make(chan *RequestQueueResponse)

	// Add the request to the queue
	var requestAlreadyInQueue bool
	client.RequestQueueMutex.Lock()
	if _, requestAlreadyInQueue = client.RequestQueue[requestQueueKey]; !requestAlreadyInQueue {
		client.RequestQueue[requestQueueKey] = make([]chan *RequestQueueResponse, 0)
	}
	client.RequestQueue[requestQueueKey] = append(client.RequestQueue[requestQueueKey], requestQueueResponseChannel)
	client.RequestQueueMutex.Unlock()

	// If the request was not already in the queue, start a worker to process it
	if !requestAlreadyInQueue {
		// client.RequestWorkerSemaphone <- struct{}{}
		go client.requestWorker(requestQueueKey)
	}

	// Wait for the response
	requestQueueResponse := <-requestQueueResponseChannel

	// Return the response
	return requestQueueResponse.fetchedUrl, requestQueueResponse.err
}


func (client *Client) requestWorker(requestQueueKey RequestQueueKey) {
	defer func() {
		// // Release the worker semaphore
   	// <-client.RequestWorkerSemaphone
	}()

	client.Logger.WithFields(logrus.Fields{
		"worker": "requestWorker",
		"url": requestQueueKey.URL,
	}).Debug("Starting request worker")

	// Fetch the URL
	headers := map[string]string{}
	if requestQueueKey.HttpHeaderIfNoneMatch != "" {
		headers["If-None-Match"] = requestQueueKey.HttpHeaderIfNoneMatch
	}
	if requestQueueKey.HttpHeaderIfModifiedSince != "" {
		headers["If-Modified-Since"] = requestQueueKey.HttpHeaderIfModifiedSince
	}
	if requestQueueKey.HttpHeaderAcceptEncoding != "" {
		headers["Accept-Encoding"] = requestQueueKey.HttpHeaderAcceptEncoding
	}
	fetchedUrl, err := client.FetchUrlDirect(requestQueueKey.URL, headers)


	// Send the result to all the requesters
	// We need to duplicate the FetchedWeb3URL response, and update its Output field
	// (each receiver will read the Output field, and we can't have multiple readers on the same io.Reader)
	client.RequestQueueMutex.Lock()
	
	// Create the shared output
	sharedOutput := &SharedOutput{
		OriginalOutput: fetchedUrl.Output,
		FetchedBytes: make([]byte, 0),
		IsEnded: false,
		Mutex: sync.Mutex{},
	}

	// For each requester, send the response
	for _, requestQueueResponseChannel := range client.RequestQueue[requestQueueKey] {
		// Duplicate the response
		fetchedUrlCopy := fetchedUrl
		fetchedUrlCopy.Output = &SharedOutputReader{
			SharedOutput: sharedOutput,
			Position: 0,
		}

		// Prepare the response
		requestQueueResponse := &RequestQueueResponse{
			fetchedUrl: &fetchedUrlCopy,
			err: err,
		}

		requestQueueResponseChannel <- requestQueueResponse
	}

	// Delete the request from the queue
	delete(client.RequestQueue, requestQueueKey)
	client.RequestQueueMutex.Unlock()
}

// When a request is shared between multiple receivers, we need to duplicate the response
// This contains the original output, and ongoing fetched bytes
type SharedOutput struct {
	// The original output
	OriginalOutput io.Reader
	// The fetched byte
	FetchedBytes []byte
	// Is ended?
	IsEnded bool

	Mutex sync.Mutex
}

// This is a io.Reader that reads from a SharedOutput
type SharedOutputReader struct {
	// The shared output
	SharedOutput *SharedOutput
	// The current position in the output
	Position int
}

func (r *SharedOutputReader) Read(p []byte) (n int, err error) {
	r.SharedOutput.Mutex.Lock()
	defer r.SharedOutput.Mutex.Unlock()

	// If trying to read after the end of the already fetched bytes, try to fetch more
	if r.Position + len(p) > len(r.SharedOutput.FetchedBytes) && !r.SharedOutput.IsEnded {
		// Make a buffer the same size as p
		buffer := make([]byte, len(p))
		// Read from the original output
		n, oerr := r.SharedOutput.OriginalOutput.Read(buffer)
		// Copy the read bytes to the fetched bytes
		r.SharedOutput.FetchedBytes = append(r.SharedOutput.FetchedBytes, buffer[:n]...)
		// If we reached the end of the original output, set the flag
		if oerr == io.EOF {
			r.SharedOutput.IsEnded = true
		}
	}

	// Copy as much bytes as possible
	n = copy(p, r.SharedOutput.FetchedBytes[r.Position:])
	r.Position += n

	// If we reached the end of the fetched bytes, and the original output is ended, return EOF
	if r.Position == len(r.SharedOutput.FetchedBytes) && r.SharedOutput.IsEnded {
		err = io.EOF
	}

	return
}


// When a RPC is returning 429, we start a worker to check if it is available again
func (client *Client) CheckTooManyRequestsStateWorker(rpc *Rpc) {
	client.RpcsMutex.RLock()
	rpcState := rpc.State
	client.RpcsMutex.RUnlock()

	// If we are not in too many requests state, return (we should enter this function only if we are in too many requests state)
	if rpcState != RpcStateTooManyRequests {
		return
	}

	client.Logger.WithFields(logrus.Fields{
		"worker": "rpcStateWorker",
		"url": rpc.Config.Url,
	}).Info("RPC is returning 429, starting worker to check if it is available again")

	for {
		// Sleep for a while
		time.Sleep(4 * time.Second)

		// Create connection
		ethClient, err := ethclient.Dial(rpc.Config.Url)
		if err != nil {
			client.Logger.WithFields(logrus.Fields{
				"worker": "rpcStateWorker",
				"url": rpc.Config.Url,
			}).Error("Failed to connect to RPC: " + err.Error())
			continue;
		}
		defer ethClient.Close()

		// Attempt to fetch the block number
		_, err = ethClient.BlockNumber(context.Background())
		if err != nil {
			client.Logger.WithFields(logrus.Fields{
				"worker": "rpcStateWorker",
				"url": rpc.Config.Url,
			}).Error("Failed to fetch block number: " + err.Error())
			continue;
		}

		// If we reached this point, the RPC is available again
		client.RpcsMutex.Lock()
		rpc.State = RpcStateAvailable
		client.RpcsMutex.Unlock()

		client.Logger.WithFields(logrus.Fields{
			"worker": "rpcStateWorker",
			"url": rpc.Config.Url,
		}).Info("RPC is available again")

		// Exit the loop
		break
	}
}