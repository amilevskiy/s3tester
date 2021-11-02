package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/template"

	"golang.org/x/time/rate"
)

const (
	// Separates items in sentence series
	textSeriesSeparator    = ", "
	addressingStyleVirtual = "virtual"
	addressingStylePath    = "path"

	randomRangeWithoutGetErr    = "the operation type for random-range must be 'get'"
	randomRangeWithRangeErr     = "random-range and range cannot be set at the same time"
	randomRangeInvalidMinMaxErr = "random-range must be in the form '<min>-<max>/<size>', where min >= 0, max > 0, and min < max"
	randomRangeInvalidSizeErr   = "random-range must be in the form '<min>-<max>/<size>', where size > 0 and size <= max-min+1"
	randomRangeInvalidFormat    = "random-range must be in the form '<min>-<max>/<size>'"
)

var (
	operationTypes = []string{"put", "multipartput", "get", "puttagging", "updatemeta", "randget", "delete", "options", "head", "restore", "copy"}
)

// Config includes configuration that applies globally to all operations
type Config struct {
	CPUProfile string
	Debug      bool
	Describe   bool
	JSON       bool
	LogDetail  string
	LogLatency string
	Retries    int
	RetrySleep int
	Workload   string

	worklist []Parameters
}

func validateConfig(config *Config) error {
	if config.Retries < 0 {
		return errors.New("Retries must be >= 0")
	}
	if config.RetrySleep < 0 {
		return errors.New("RetrySleep must be >= 0")
	}
	return nil
}

// Parameters includes configuration that applies to an individual set of operations
type Parameters struct {
	AddressingStyle   string      `json:"addressing-style,omitempty"`
	Bucket            string      `json:"bucket,omitempty"`
	CommandAfter      string      `json:"command-after,omitempty"`
	CommandBefore     string      `json:"command-before,omitempty"`
	Concurrency       int         `json:"concurrency,omitempty"`
	CopySourceBucket  string      `json:"copy-source-bucket,omitempty"`
	Days              int64       `json:"-"`
	Duration          int         `json:"duration,omitempty"`
	Endpoint          string      `json:"endpoint,omitempty"`
	Header            headerFlags `json:"header,omitempty"`
	Lockstep          bool        `json:"-"`
	Metadata          string      `json:"metadata,omitempty"`
	MetadataDirective string      `json:"metadata-directive,omitempty"`
	NoSignRequest     bool        `json:"no-sign-request,omitempty"`
	Operation         string      `json:"operation,omitempty"`
	Overwrite         int         `json:"-"`
	PartSize          int64       `json:"partsize,omitempty"`
	Prefix            string      `json:"prefix,omitempty"`
	Profile           string      `json:"profile,omitempty"`
	QueryParams       string      `json:"query-params,omitempty"`
	RandomRange       string      `json:"random-range,omitempty"`
	Range             string      `json:"range,omitempty"`
	RateLimit         float64     `json:"ratelimit,omitempty"`
	Region            string      `json:"-"`
	Repeat            int         `json:"repeat,omitempty"`
	MixedWorkload     string      `json:"-"`
	Requests          int         `json:"requests,omitempty"`
	Size              int64       `json:"size,omitempty"`
	Tagging           string      `json:"tagging,omitempty"`
	TaggingDirective  string      `json:"tagging-directive,omitempty"`
	Tier              string      `json:"-"`
	UniformDist       string      `json:"-"`
	Verify            int         `json:"-"`
	Wait              int         `json:"wait,omitempty"`

	// Internal

	endpoints       []string
	ratePerSecond   rate.Limit
	attempts        int
	min             int64
	max             int64
	randomRangeMin  int64
	randomRangeMax  int64
	randomRangeSize int64
}

// NewParameters returns default parameters
func NewParameters() *Parameters {
	return &Parameters{Header: make(headerFlags)}
}

func (params *Parameters) hasRandomSize() bool {
	return params.max != 0 && params.min != 0
}

func (params *Parameters) hasRandomRange() bool {
	return params.RandomRange != ""
}

// Copy performs a shallow copy of Parameters
func (params *Parameters) Copy() *Parameters {
	dst := *params
	dst.Header = make(headerFlags)
	for k, v := range params.Header {
		dst.Header[k] = v
	}
	return &dst
}

// Merge a collection of fields by json tag name into a parameters instance while ignoring fields
// if needed.
func (params *Parameters) Merge(fields map[string]interface{}, ignore []string) error {
	fieldsCopy := map[string]interface{}{}
	for k, v := range fields {
		fieldsCopy[k] = v
	}
	for _, v := range ignore {
		delete(fieldsCopy, v)
	}
	b, err := json.Marshal(fieldsCopy)
	if err != nil {
		return err
	}
	err = json.Unmarshal(b, params)
	if err != nil {
		return err
	}
	return nil
}

type headerFlags map[string]string

func (hf *headerFlags) String() string {
	return fmt.Sprintf("%v", map[string]string(*hf))
}

func (hf *headerFlags) Set(v string) error {
	keyval := strings.SplitN(v, ":", 2)
	if len(keyval) != 2 {
		return fmt.Errorf("Failed to parse header: %v", v)
	}
	// Overrides duplicates.
	(*hf)[keyval[0]] = keyval[1]
	return nil
}

// makeSlice is a utility function for templates to generate array/slice literals
func makeSlice(args ...interface{}) []interface{} {
	return args
}

func parse(args []string) (*Config, error) {
	flags := flag.NewFlagSet(os.Args[0], flag.ExitOnError)

	// Config fields
	config := &Config{}
	flags.StringVar(&config.CPUProfile, "cpuprofile", "", "write cpu profile to file")
	flags.BoolVar(&config.Debug, "debug", false, "Print response body on request failure")
	flags.BoolVar(&config.Describe, "describe", false, "Instead of running tests, show the consolidated list of test parameters that will be used when a test is run.")
	flags.BoolVar(&config.JSON, "json", false, "The result will be printed out in JSON format if this flag exists")
	flags.StringVar(&config.LogDetail, "logdetail", "", "write detailed log to file")
	flags.StringVar(&config.LogLatency, "loglatency", "", "write latency histogram to file. Latencies exceeding 10 hours are not included in the histogram.")
	flags.IntVar(&config.Retries, "retries", 0, "Number of retry attempts. Default is 0.")
	flags.IntVar(&config.RetrySleep, "retrysleep", 0, "How long to sleep in between each retry in milliseconds. Default (0) is to use the default retry method which is an exponential backoff.")
	flags.StringVar(&config.Workload, "workload", "", "File path to a JSON file that describes a workload to be run. The file is parsed with the Go template package and must produce JSON that is valid according to the workload schema.")

	// Parameter fields
	params := NewParameters()
	flags.StringVar(&params.AddressingStyle, "addressing-style", addressingStylePath, "whether to use virtual-hosted style addresses (bucket name is in the hostname) or path-style addresses (bucket name is part of the path). Value must be one of 'virtual' or 'path'")
	flags.StringVar(&params.Bucket, "bucket", "test", "bucket name (needs to exist)")
	flags.IntVar(&params.Concurrency, "concurrency", 1, "Maximum concurrent requests.")
	flags.StringVar(&params.CopySourceBucket, "copy-source-bucket", "", "The name of the source bucket to use for copying objects.")
	flags.Int64Var(&params.Days, "days", 1, "The number of days that the restored object will be available for")
	flags.IntVar(&params.Duration, "duration", 0, "Test duration in seconds. Duration can be specified for these operations: options, put, multipartput, and delete")
	flags.StringVar(&params.Endpoint, "endpoint", "https://127.0.0.1:18082", "target endpoint(s). If multiple endpoints are specified separate them with a ','. Note: the concurrency must be a multiple of the number of endpoints.")
	flags.Var(&params.Header, "header", "Specify one or more headers of the form \"<header-name>: <header-value>\".")
	flags.BoolVar(&params.Lockstep, "lockstep", false, "Force all threads to advance at the same rate rather than run independently")
	flags.StringVar(&params.Metadata, "metadata", "", "The metadata to use for the objects. The string must be formatted as such: 'key1=value1&key2=value2'. Used for put, updatemeta, multipartput, putget and putget9010r.")
	flags.StringVar(&params.MetadataDirective, "metadata-directive", directiveCopy, "Specifies whether the metadata is copied from the source object or if it is replaced with the metadata provided in the object copy request. Value must be one of 'COPY' or 'REPLACE'")
	flags.BoolVar(&params.NoSignRequest, "no-sign-request", false, "Do not sign requests. Credentials will not be loaded if this argument is provided.")
	flags.StringVar(&params.Operation, "operation", "put", "operation type: "+strings.Join(operationTypes, textSeriesSeparator))
	flags.IntVar(&params.Overwrite, "overwrite", 0, "Turns a PUT/GET/HEAD into an operation on the same s3 key. (1=all writes/reads are to same object, 2=threads clobber each other but each write/read is to unique objects).")
	flags.Int64Var(&params.PartSize, "partsize", 5*(1<<20), "Size of each part (min 5MiB); only has an effect when a multipart put is used")
	flags.StringVar(&params.Prefix, "prefix", "testobject", "object name prefix")
	flags.StringVar(&params.Profile, "profile", "", "Use a specific profile from AWS CLI credential file (https://docs.aws.amazon.com/cli/latest/userguide/cli-configure-profiles.html).")
	flags.StringVar(&params.QueryParams, "query-params", "", "Specify one or more custom query parameters of the form \"<queryparam-name>=<queryparam-value>\" or \"<queryparam-name>\" separated by ampersands.")
	flags.StringVar(&params.RandomRange, "random-range", "", "Used to perform random range GET requests. Format is <min>-<max>/<size>, where <size> is the number of bytes per GET request, and <min>-<max> is an inclusive byte range within the object . Ex: Use 0-399/100 to perform random 100-byte reads within the first 400 bytes of an object.")
	flags.StringVar(&params.Range, "range", "", "Specify the range header for GET requests. Format is bytes=<min>-<max>, where <min>-<max> is an inclusive byte range within the object. Ex: Use -range=bytes=0-4095 to read the first 4096 bytes of an object.")
	flags.Float64Var(&params.RateLimit, "ratelimit", math.MaxFloat64, "the total number of operations per second across all threads")
	flags.StringVar(&params.Region, "region", "us-east-1", "Region to send requests to")
	flags.IntVar(&params.Repeat, "repeat", 0, "Repeat each S3 operation this many times, by default doesn't repeat (i.e. repeat=0)")
	flags.StringVar(&params.MixedWorkload, "mixed-workload", "", `Path to a JSON file that specifies a mixture of operations. A sample of the mixed workload format is:
{"mixedWorkload": [
	{"operationType":"put", "ratio":25},
	{"operationType":"head", "ratio":25},
	{"operationType":"get", "ratio":25},
	{"operationType":"delete", "ratio":25}
]}
Note: Requests are generated in the same order that you specify operations. That is, if you specify a delete followed by a put, but have no existing objects to delete, all of the deletes will fail.`)
	flags.IntVar(&params.Requests, "requests", 1000, "Total number of requests.")
	flags.Int64Var(&params.Size, "size", 30*1024, "Object size.")
	flags.StringVar(&params.Tagging, "tagging", "", "The tag-set for the object. The tag-set must be formatted as such: 'tag1=value1&tag2=value2'. Used for put, puttagging, putget and putget9010r.")
	flags.StringVar(&params.TaggingDirective, "tagging-directive", directiveCopy, "Specifies whether the object tag-set is copied from the source object or if it is replaced with the tag-set provided in the object copy request. Value must be one of 'COPY' or 'REPLACE'")
	flags.StringVar(&params.Tier, "tier", "standard", "The retrieval option for restoring an object. One of expedited, standard, or bulk. AWS default option is standard if not specified")
	flags.StringVar(&params.UniformDist, "uniformDist", "", "Generates a uniform distribution of object sizes given a min-max size (10-20)")
	flags.IntVar(&params.Verify, "verify", 0, "Verify the retrieved data on a get operation (0=disable verify(default), 1=normal put data, 2=multipart put data). If verify=2, partsize is required and default partsize is set to 5242880.")
	flags.SetOutput(os.Stdout)

	flags.Usage = func() {
		fmt.Println("\nVersion:", version)
		fmt.Println("This tool is for generating high performance S3 load against an S3 server.")
		fmt.Println("It reads credentials from the environment variables AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY or loads credentials generated by AWS CLI.")
		fmt.Println()
		fmt.Println("Key naming is unique per key unless the 'overwrite' option is used. The naming is as follows:")
		fmt.Println("    Key names are equal to \"<prefix>-N-M\" where N is 0..concurrency-1 and M is the request within that connection.")
		fmt.Println("This means all the various client operations (GET, PUT, DELETE, etc) will use the same object names assuming the same parameters are used.")
		fmt.Println()
		fmt.Println("You can control how many client machine cores the tester uses by setting the GOMAXPROCS env variable, e.g. to use 8 cores:")
		fmt.Println("    export GOMAXPROCS=8")
		fmt.Println()
		fmt.Println("Each concurrent stream is done on a persistent HTTP connection.")
		fmt.Println("You should expect the very first request on a persistent to take a little longer due to TLS handshake.")
		fmt.Println("You can see this effect by setting requests=concurrency")
		fmt.Println()
		fmt.Println("Client performance will vary based on your machine but as a rule of thumb a modern Core i7")
		fmt.Println("should be able to generate about 5000 30K PUTs/second (should saturate a 1GBps network interface)")
		fmt.Println("and 20000 DELETE requests/s (with GOMAXPROCS=# of cores and concurrency>=# of cores)")
		fmt.Println()
		fmt.Println("Usage of", os.Args[0])
		flags.PrintDefaults()
		fmt.Println()
		fmt.Println("Note: When options are provided through multiple means, the priority order from highest to lowest is: command-line, workload, global, defaults")
	}

	// Backwards compatibility
	reducedRedundancy := flags.Bool("rr", false, "Reduced redundancy storage for PUT requests")
	consistencyControlTypes := []string{"all", "available", "strong-global", "strong-site", "read-after-new-write", "weak"}
	consistencyControl := flags.String("consistency", "", "The StorageGRID consistency control to use for all requests. Does nothing against non StorageGRID systems. ("+strings.Join(consistencyControlTypes, textSeriesSeparator)+")")

	flags.Parse(args)
	// Backwards compatibility
	if *reducedRedundancy {
		params.Header["x-amz-storage-class"] = "REDUCED_REDUNDANCY"
	}
	if *consistencyControl != "" {
		if !contains(consistencyControlTypes, *consistencyControl) {
			return nil, fmt.Errorf("%s consistency is not one of: %s", *consistencyControl, strings.Join(consistencyControlTypes, textSeriesSeparator))
		}
		params.Header["Consistency-Control"] = *consistencyControl
	}

	// validation after parsing
	err := validateConfig(config)
	if err != nil {
		return nil, err
	}

	if config.Workload != "" {
		t, err := template.New(filepath.Base(config.Workload)).
			Funcs(template.FuncMap{"makeSlice": makeSlice}).
			ParseFiles(config.Workload)
		if err != nil {
			return nil, fmt.Errorf("Failed to parse workload template: %v", err)
		}
		b := &bytes.Buffer{}
		if err = t.Execute(b, nil); err != nil {
			return nil, fmt.Errorf("Failed to execute workload template: %v", err)
		}
		ignoreFlags := make([]string, 0) // Flags to ignore when merging the workload since command-line is higher priority
		flags.Visit(func(f *flag.Flag) {
			ignoreFlags = append(ignoreFlags, f.Name)
		})

		// Backwards compatibility
		if *reducedRedundancy || *consistencyControl != "" {
			ignoreFlags = append(ignoreFlags, "header")
		}

		config.worklist, err = createWorklist(*params, b.Bytes(), ignoreFlags)
		if err != nil {
			return nil, err
		}
	} else {
		err = setupParam(params)
		if err != nil {
			return nil, err
		}
		config.worklist = []Parameters{*params}
	}

	return config, nil
}

// Workload specifies a sequence of operations to run
type Workload struct {
	// Global contains field values to use for any item in the workload that doesn't specify them
	Global *Parameters `json:"global,omitempty"`

	// Workload is a sequence of parameters to evaluate in-order
	Workload []Parameters `json:"workload,omitempty"`
}

const (
	globalField   = "global"
	workloadField = "workload"
)

func createWorklist(params Parameters, workloadData []byte, ignore []string) ([]Parameters, error) {
	worklist := make([]Parameters, 0)
	if workloadData == nil {
		return worklist, nil
	}
	var workload map[string]interface{}
	if err := json.Unmarshal(workloadData, &workload); err != nil {
		return nil, fmt.Errorf("Failed parsing workload file: %v", err)
	}
	var global map[string]interface{}
	if g, ok := workload[globalField]; ok {
		if global, ok = g.(map[string]interface{}); !ok {
			return nil, fmt.Errorf("Failed parsing workload: %q field does not match schema", globalField)
		}
	}
	globalParams := params
	err := globalParams.Merge(global, ignore)
	if err != nil {
		return nil, fmt.Errorf("failed to merge global workload fields: %v", err)
	}

	var tests []interface{}
	if t, ok := workload[workloadField]; ok {
		if tests, ok = t.([]interface{}); !ok {
			return nil, fmt.Errorf("Failed parsing workload: %q field does not match schema", workloadField)
		}
	}

	for i, v := range tests {
		test, ok := v.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("Failed parsing workload: test at position %d does not match schema", i)
		}

		testParams := globalParams.Copy()
		err = testParams.Merge(test, ignore)
		if err != nil {
			return nil, fmt.Errorf("failed to merge workload fields: %v", err)
		}

		err = setupParam(testParams)
		if err != nil {
			return nil, fmt.Errorf("failed to verify and initialize parameters: %v", err)
		}
		worklist = append(worklist, *testParams)
	}

	return worklist, nil
}

func setupParam(args *Parameters) error {
	// comparing with its default value
	isDurationSet := args.Duration != 0
	isRequestsSet := args.Requests != 1000
	if !isDurationSet && args.Requests <= 0 {
		return errors.New("Number of requests must be > 0")
	}

	if isDurationSet && args.Duration < 0 {
		return errors.New("Duration must be >= 0")
	}

	if isDurationSet {
		if args.Operation == "delete" {
			return fmt.Errorf("Duration not supported for operation %q", args.Operation)
		}

		ops := []string{"options", "put", "multipartput"} // Ops either creating or not needing existing objects
		if isRequestsSet && contains(ops, args.Operation) {
			return fmt.Errorf("Using duration with requests is not supported for operation %q", args.Operation)
		}

		if !isRequestsSet && !contains(ops, args.Operation) {
			return fmt.Errorf("Using duration without requests is not supported for operation %q", args.Operation)
		}
	}

	if args.Wait < 0 {
		return errors.New("Wait must be >= 0")
	}

	args.ratePerSecond = rate.Limit(args.RateLimit)

	if !contains(operationTypes, args.Operation) {
		return fmt.Errorf("operation type must be one of: %s", strings.Join(operationTypes, textSeriesSeparator))
	}

	if args.Concurrency <= 0 {
		return errors.New("Concurrency must be > 0")
	}

	if args.Operation == "copy" {
		if args.CopySourceBucket == "" {
			return errors.New("the following arguments are required: --copy-source-bucket")
		}

		if !isValidDirective(args.TaggingDirective) {
			return fmt.Errorf("tagging-directive must be one of %s or %s", directiveCopy, directiveReplace)
		}

		if !isValidDirective(args.MetadataDirective) {
			return fmt.Errorf("metadata-directive must be one of %s or %s", directiveCopy, directiveReplace)
		}
	} else if args.CopySourceBucket != "" {
		return errors.New("--copy-source-bucket can only be used with a copy operation")
	}

	if !isDurationSet && args.Requests < args.Concurrency {
		return errors.New("Number of requests must be greater than or equal to concurrency")
	}

	if args.Operation == "multipartput" {
		if args.PartSize < 5*(1<<20) {
			return errors.New("Part size should be 5MiB at minimum")
		}
		if int(math.Ceil(float64(args.Size)/float64(args.PartSize))) > 10000 {
			return errors.New("The multipart upload will use too many parts (max 10000)")
		}
	}

	if args.Repeat < 0 {
		return errors.New("Repeat must be >= 0")
	}

	// attempts indicate the number of times we perform S3 operation, the default attempts is 1
	args.attempts = 1 + args.Repeat

	if args.NoSignRequest && args.Profile != "" {
		return errors.New("Cannot load credential profile if argument nosign is provided")
	}

	if args.UniformDist != "" && (args.Operation != "put" && args.Operation != "get") {
		return errors.New("UniformDist can only be used with a put or get operation")
	}

	if !strings.EqualFold(args.Tier, "Standard") && !strings.EqualFold(args.Tier, "Expedited") && !strings.EqualFold(args.Tier, "Bulk") {
		return errors.New("Restore tier must be one of Standard, Expedited, or Bulk. Case Insensitive")
	}

	if args.Days < 1 {
		return errors.New("Restore days must be a positive, non-zero integer")
	}

	var err error

	if args.endpoints, err = generateEndpoints(args.Endpoint); err != nil {
		return err
	}

	if args.AddressingStyle != addressingStyleVirtual && args.AddressingStyle != addressingStylePath {
		return errors.New("addressing style must be one of 'virtual' or 'path'")
	}

	if (args.Concurrency)%len(args.endpoints) != 0 {
		return errors.New("The concurrency must be multiple of endpoint list length")
	}

	args.min, args.max, err = extractRangeMinMax(args.UniformDist)
	if err != nil {
		return errors.New("UniformDist must be in form 'min-max', where min and max are > 0,  min < max, and have a put or get as operation type")
	}

	// validate random-range
	if args.RandomRange != "" && args.Operation != "get" {
		return errors.New(randomRangeWithoutGetErr)
	}
	if args.RandomRange != "" && args.Range != "" {
		return errors.New(randomRangeWithRangeErr)
	}

	if args.RandomRange != "" {
		randomRangeParts := strings.Split(args.RandomRange, "/")
		if len(randomRangeParts) != 2 {
			return errors.New(randomRangeInvalidFormat)
		}

		args.randomRangeMin, args.randomRangeMax, err = extractRangeMinMax(randomRangeParts[0])
		if err != nil {
			return errors.New(randomRangeInvalidMinMaxErr)
		}

		args.randomRangeSize, err = strconv.ParseInt(randomRangeParts[1], 10, 64)
		if err != nil || args.randomRangeSize <= 0 || args.randomRangeSize > (args.randomRangeMax-args.randomRangeMin+1) {
			return errors.New(randomRangeInvalidSizeErr)
		}
	}

	if args.Range != "" && args.Operation != "get" {
		return errors.New("Operation type for range read must be get")
	}

	if args.Range != "" {
		rangeParts := strings.Split(args.Range, "=")
		if len(rangeParts) != 2 {
			return errors.New("Range must be in the form bytes=<min>-<max>")
		}

		if rangeParts[0] != "bytes" {
			return errors.New("Range must be in the form bytes=<min>-<max>")
		}

		_, _, err := extractRangeMinMax(rangeParts[1])
		if err != nil {
			return err
		}
	}

	if _, err := url.ParseQuery(args.QueryParams); err != nil {
		return fmt.Errorf("Unable to parse query parameters: %v", err)
	}

	return nil
}

func extractRangeMinMax(arg string) (min int64, max int64, err error) {
	if arg == "" {
		return 0, 0, nil
	}

	boundaries := strings.Split(arg, "-")
	if len(boundaries) != 2 {
		return 0, 0, errors.New("argument is in invalid format")
	}

	min, err = strconv.ParseInt(boundaries[0], 10, 64)
	if err != nil || min < 0 {
		return 0, 0, errors.New(randomRangeInvalidMinMaxErr)
	}

	max, err = strconv.ParseInt(boundaries[1], 10, 64)
	if err != nil || max <= 0 {
		return 0, 0, errors.New(randomRangeInvalidMinMaxErr)
	}

	if min > max {
		return 0, 0, errors.New("max must be larger than min")
	}

	return min, max, nil
}

// generateEndpoints validates the input endpoint string by rejecting invalid/duplicate
// URLs and returning the endpoint collection
func generateEndpoints(endpoint string) ([]string, error) {
	endpointSet := make(map[string]struct{})
	endpoints := make([]string, 0)
	for _, endpoint := range strings.Split(endpoint, ",") {
		trimEndpoint := strings.Trim(endpoint, " ")
		if _, err := url.ParseRequestURI(trimEndpoint); err != nil {
			return nil, errors.New("URL \"" + trimEndpoint + "\" is not a valid endpoint")
		}
		// check if map contains this key to identify duplicate URLs
		if _, hasKey := endpointSet[trimEndpoint]; hasKey {
			return nil, errors.New("URL \"" + trimEndpoint + "\" is a duplicate endpoint")
		}
		endpointSet[trimEndpoint] = struct{}{}
		endpoints = append(endpoints, trimEndpoint)
	}
	return endpoints, nil
}

func contains(slice []string, elem string) bool {
	for _, v := range slice {
		if v == elem {
			return true
		}
	}
	return false
}

func remove(slice []string, elem string) []string {
	for i, v := range slice {
		if v == elem {
			return append(slice[:i], slice[i+1:]...)
		}
	}
	return slice
}

func isValidDirective(directive string) bool {
	return directive == directiveCopy || directive == directiveReplace
}
