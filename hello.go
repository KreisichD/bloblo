package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/patrickmn/go-cache"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

const (
	presignExpirationMinutes = 5
)

var (
	logger          *zap.Logger
	listenAddress   string
	s3BucketName    string
	s3Client        *s3.Client
	s3PresignClient *s3.PresignClient
	s3Uploader      *manager.Uploader

	upstreamUrl  *url.URL
	preserveHost bool

	connectEndpointCache *cache.Cache
	tokenEndpointCache   *cache.Cache
	cachedBlobDigests    map[string]bool
)

func initLogger() {

	infoLevel := zap.LevelEnablerFunc(func(level zapcore.Level) bool {
		return level == zapcore.InfoLevel
	})

	errorLevel := zap.LevelEnablerFunc(func(level zapcore.Level) bool {
		return level == zapcore.ErrorLevel || level == zapcore.FatalLevel || level == zapcore.PanicLevel
	})

	stdout := zapcore.Lock(os.Stdout)
	stderr := zapcore.Lock(os.Stderr)

	core := zapcore.NewTee(
		zapcore.NewCore(
			zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig()),
			stdout,
			infoLevel,
		),
		zapcore.NewCore(
			zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig()),
			stderr,
			errorLevel,
		),
	)

	logger = zap.New(core)
}

func init() {
	listenAddress = os.Getenv("BLOBLO_LISTEN_ADDR")
	if listenAddress == "" {
		listenAddress = ":7777"
	}

	s3BucketName = os.Getenv("BLOBLO_S3_BUCKET_NAME")
	if s3BucketName == "" {
		s3BucketName = "sample-bucket"
	}

	upstreamRawUrl := os.Getenv("BLOBLO_UPSTREAM_URL")
	if upstreamRawUrl == "" {
		upstreamRawUrl = "http://localhost:6666"
	}

	preserveHost = os.Getenv("BLOBLO_PRESERVE_HOST") == "true"

	var err error
	upstreamUrl, err = url.Parse(upstreamRawUrl)
	if err != nil {
		logger.Fatal("Can't parse the upstream url", zap.String("error", err.Error()))
	}

	useLocalStack := os.Getenv("BLOBLO_USE_LOCALSTACK")
	var awsConfig aws.Config
	if useLocalStack == "true" {
		localStackResolver := aws.EndpointResolverFunc(func(service, region string) (aws.Endpoint, error) {
			localstackUrl := "http://localhost:4566"

			return aws.Endpoint{
				PartitionID:   "aws",
				URL:           localstackUrl,
				SigningRegion: "us-east-1",
			}, nil
		})
		awsConfig, err = config.LoadDefaultConfig(context.TODO(),
			config.WithEndpointResolver(localStackResolver))
	} else {
		awsConfig, err = config.LoadDefaultConfig(context.TODO())
	}
	if err != nil {
		logger.Error("Error loading AWS connection configuration", zap.String("error", err.Error()))
		return
	}

	cacheExpirationTime, err := strconv.Atoi(os.Getenv("BLOBLO_CACHE_EXPIRATION_MINUTES"))
	if err != nil {
		cacheExpirationTime = 10
	}

	connectEndpointCache = cache.New(time.Duration(cacheExpirationTime)*time.Minute, 10*time.Minute)
	tokenEndpointCache = cache.New(time.Duration(cacheExpirationTime)*time.Minute, 10*time.Minute)

	s3Client = s3.NewFromConfig(awsConfig, func(opts *s3.Options) {
		opts.UsePathStyle = true
	})

	s3PresignClient = s3.NewPresignClient(s3Client)

	s3Uploader = manager.NewUploader(s3Client)
}

func presignBlob(blobDigest string) string {
	urlStr, err := s3PresignClient.PresignGetObject(
		context.TODO(),
		&s3.GetObjectInput{
			Bucket: aws.String(s3BucketName),
			Key:    aws.String(blobDigest),
		}, s3.WithPresignExpires(presignExpirationMinutes*time.Minute))

	if err != nil {
		logger.Error("Failed to sign request", zap.String("error", err.Error()))
	}

	return urlStr.URL
}

func blobInCache(blobDigest string) bool {
	_, err := s3Client.HeadObject(context.TODO(), &s3.HeadObjectInput{Bucket: &s3BucketName, Key: &blobDigest})
	return err == nil
}

type blobloProxy struct {
	proxy *httputil.ReverseProxy
}

func getUpstreamRequest(req *http.Request) *http.Request {
	upstreamReq := req.Clone(req.Context())
	upstreamReq.RequestURI = ""
	upstreamReq.Host = upstreamUrl.Host
	upstreamReq.URL.Host = upstreamUrl.Host
	upstreamReq.URL.Scheme = upstreamUrl.Scheme
	return upstreamReq
}

func performUpstreamRequest(req *http.Request) *http.Response {
	upstreamReq := getUpstreamRequest(req)
	response, err := http.DefaultClient.Do(upstreamReq)
	if err != nil {
		logger.Error("Failed to reach the upstream", zap.String("error", err.Error()))
	}
	logger.Info("Response", zap.String("response", response.Status), zap.String("request", req.RequestURI))
	return response
}

type TokenData struct {
	Token string `json:"token"`
}

func checkJsonTokenData(responseData []byte) bool {
	var v TokenData
	if err := json.Unmarshal(responseData, &v); err != nil {
		logger.Error("Response is not a JSON, processing", zap.String("error", err.Error()))
		return false
	}
	logger.Info("Checking Json token", zap.String("token", v.Token))

	regexp_check, err := regexp.MatchString("^DockerToken.[0-9a-f]+-[0-9a-f]+-[0-9a-f]+-[0-9a-f]+-[0-9a-f]+$", v.Token)
	if err != nil {
		logger.Error("Error matching token pattern", zap.String("error", err.Error()))
		return false
	}
	return regexp_check
}

// func copyHeaders(from http.Header, to http.Header) {
// 	for key, values := range from {
// 		for _, value := range values {
// 			to.Add(key, value)
// 		}
// 	}
// }

func cachedEndpointHandler(w http.ResponseWriter, req *http.Request, cacheStore *cache.Cache, checkToken bool) bool {
	authHeader := req.Header.Get("Authorization")
	cacheValue, ok := cacheStore.Get(authHeader)
	if ok {
		logger.Info("Serving cached response", zap.String("request", req.RequestURI))
		if checkToken && !checkJsonTokenData(cacheValue.([]byte)) {
			logger.Error("Cached response is not a valid token", zap.String("response", string(cacheValue.([]byte))))
			return false
		}
		w.Write(cacheValue.([]byte))
		return true
	}

	response := performUpstreamRequest(req)

	defer response.Body.Close()
	bodyData, err := io.ReadAll(response.Body)
	if err != nil {
		logger.Error("Failed to read response body", zap.String("error", err.Error()))
		w.WriteHeader(http.StatusInternalServerError)
		return true
	}

	if response.StatusCode == http.StatusOK {
		if checkToken && !checkJsonTokenData(bodyData) {
			logger.Error("Response is not valid token, do not save in cache, try proxtying to nexus instead", zap.String("response", string(cacheValue.([]byte))))
			return false
		}
		cacheStore.Set(authHeader, bodyData, cache.DefaultExpiration)
	}
	return false
}

func (rl *blobloProxy) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	logger.Info("Incoming request", zap.String("request", req.RequestURI), zap.String("method", req.Method))
	nexus_request_performed := false

	if req.Method == http.MethodGet {
		pathElements := strings.Split(req.RequestURI, "/")
		nexus_request_performed = rl.ServeCachedDockerApiEndpoint(w, req, pathElements) || rl.ServeDockerBlob(w, req, pathElements)
	}

	if !nexus_request_performed {
		rl.proxy.ServeHTTP(w, req)
	}
}

func (rl *blobloProxy) ServeCachedDockerApiEndpoint(w http.ResponseWriter, req *http.Request, pathElements []string) bool {
	if len(pathElements) == 3 && pathElements[1] == "v2" && pathElements[2] == "" {
		logger.Info("Request is for a Docker connectivity check", zap.String("request", req.RequestURI))
		return cachedEndpointHandler(w, req, connectEndpointCache, false)
	}

	lastPathElementWithoutParams := strings.Split(pathElements[len(pathElements)-1], "?")[0]
	if len(pathElements) == 3 && lastPathElementWithoutParams == "token" {
		logger.Info("Request is for a Docker token request", zap.String("request", req.RequestURI))
		return cachedEndpointHandler(w, req, tokenEndpointCache, true)
	}

	return false
}

func (rl *blobloProxy) ServeDockerBlob(w http.ResponseWriter, req *http.Request, pathElements []string) bool {
	if len(pathElements) > 2 && pathElements[len(pathElements)-2] == "blobs" {
		blobDigest := pathElements[len(pathElements)-1]

		headReq := getUpstreamRequest(req)
		headReq.Method = http.MethodHead
		head_response, err := http.DefaultClient.Do(headReq)
		if err != nil {
			logger.Error("Failed to reach the upstream", zap.String("error", err.Error()))
			w.WriteHeader(http.StatusInternalServerError)
			return false
		}

		defer head_response.Body.Close()

		if head_response.StatusCode == http.StatusOK {
			if blobInCache(blobDigest) {
				user, _, _ := headReq.BasicAuth()
				logger.Info("Serving blob from cache", zap.String("digest", blobDigest), zap.String("user", user), zap.String("action", "serve_blob"))
				http.Redirect(w, req, presignBlob(blobDigest), http.StatusFound)
				return true
			} else { // upload the blob to cache and return the layer to the client
				upstream_req_response := performUpstreamRequest(req)
				defer upstream_req_response.Body.Close()
				teeReader := io.TeeReader(upstream_req_response.Body, w)

				logger.Info("Uploading blob to cache", zap.String("digest", blobDigest), zap.String("action", "upload_blob"))
				_, err = s3Uploader.Upload(
					context.TODO(),
					&s3.PutObjectInput{
						Bucket:            &s3BucketName,
						Key:               &blobDigest,
						Body:              teeReader,
						ChecksumAlgorithm: types.ChecksumAlgorithmSha256,
					})
				if err != nil {
					logger.Error("Error uploading blob", zap.String("digest", blobDigest), zap.String("error", err.Error()))
				}
				return true
			}
		}
	}
	return false
}

func main() {
	initLogger()
	defer logger.Sync()
	logger.Sugar().Infof("Hello, World! I will use %s as my upstream and listen on %s", upstreamUrl, listenAddress)
	logger.Sugar().Infof("I will keep my blobs in the bucket named %s", s3BucketName)
	logger.Info("Please keep your fingers crossed ;)")

	_, err := s3Client.GetBucketLocation(context.TODO(), &s3.GetBucketLocationInput{Bucket: &s3BucketName})
	if err != nil {
		logger.Error("The AWS configuration seems to be invalid", zap.String("error", err.Error()))
		logger.Fatal(err.Error())
	}

	logger.Info("Running bloblo proxy")

	//a custom Director is needed, as we have to set the host header
	r := blobloProxy{proxy: &httputil.ReverseProxy{Director: func(req *http.Request) {
		req.URL.Scheme = upstreamUrl.Scheme
		req.URL.Host = upstreamUrl.Host
		if !preserveHost {
			req.Host = upstreamUrl.Host
		}
		if _, ok := req.Header["User-Agent"]; !ok {
			// explicitly disable User-Agent so it's not set to default value
			req.Header.Set("User-Agent", "")
		}
	},
		ErrorLog: zap.NewStdLog(logger),
	}}
	err = http.ListenAndServe(listenAddress, &r)
	if err != nil {
		logger.Fatal(err.Error())
	}
}
