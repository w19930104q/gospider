package core

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	jsoniter "github.com/json-iterator/go"

	"github.com/gocolly/colly/v2"
	"github.com/gocolly/colly/v2/extensions"
	"github.com/jaeles-project/gospider/stringset"
	"github.com/spf13/cobra"
)

var DefaultHTTPTransport = &http.Transport{
	DialContext: (&net.Dialer{
		Timeout: 10 * time.Second,
		// Default is 15 seconds
		KeepAlive: 30 * time.Second,
	}).DialContext,
	MaxIdleConns:    100,
	MaxConnsPerHost: 1000,
	IdleConnTimeout: 30 * time.Second,

	// ExpectContinueTimeout: 1 * time.Second,
	// ResponseHeaderTimeout: 3 * time.Second,
	// DisableCompression:    false,
	TLSClientConfig: &tls.Config{InsecureSkipVerify: true, Renegotiation: tls.RenegotiateOnceAsClient},
}

type Crawler struct {
	cmd                 *cobra.Command
	C                   *colly.Collector
	LinkFinderCollector *colly.Collector
	Output              *Output

	subSet  *stringset.StringFilter
	awsSet  *stringset.StringFilter
	jsSet   *stringset.StringFilter
	urlSet  *stringset.StringFilter
	formSet *stringset.StringFilter

	site       *url.URL
	domain     string
	Input      string
	Quiet      bool
	JsonOutput bool
	length     bool
	raw        bool
	subs       bool

	filterLength_slice []int

	// wg tracks the full lifetime of a crawling job:
	// every entry-point goroutine (Start, robots, sitemap, other-sources) and
	// every request enqueued into either Collector (including all requests
	// derived recursively from callbacks) is counted here. Wait blocks until
	// every one of them has settled, so no request or output can happen after
	// Wait returns.
	wg sync.WaitGroup

	// otherSourceFn is the third-party URL provider. It is a field so tests can
	// replace it with an offline stub instead of hitting external services.
	otherSourceFn func(domain string, includeSubs bool) []string

	// closeOnce makes sure the output file is closed exactly one time, and only
	// after every piece of work (and therefore every write) has finished.
	closeOnce sync.Once

	// doneOnce guards against running the same Crawler more than once.
	doneOnce sync.Once

	// activityTransport is an optional recording RoundTripper used by tests to
	// assert that no network activity remains after Wait. Nil in production.
	activityTransport ActivityRecorder
}

// ActivityRecorder is the test-side view of a transport that records every
// request. Production code never provides one.
type ActivityRecorder interface {
	http.RoundTripper
	Inflight() int32
	Snapshot() []string
}

type SpiderOutput struct {
	Input      string `json:"input"`
	Source     string `json:"source"`
	OutputType string `json:"type"`
	Output     string `json:"output"`
	StatusCode int    `json:"status"`
	Length     int    `json:"length"`
}

func NewCrawler(site *url.URL, cmd *cobra.Command) *Crawler {
	domain := GetDomain(site)
	if domain == "" {
		Logger.Error("Failed to parse domain")
		os.Exit(1)
	}
	Logger.Infof("Start crawling: %s", site)

	quiet, _ := cmd.Flags().GetBool("quiet")
	jsonOutput, _ := cmd.Flags().GetBool("json")
	maxDepth, _ := cmd.Flags().GetInt("depth")
	concurrent, _ := cmd.Flags().GetInt("concurrent")
	delay, _ := cmd.Flags().GetInt("delay")
	randomDelay, _ := cmd.Flags().GetInt("random-delay")
	length, _ := cmd.Flags().GetBool("length")
	raw, _ := cmd.Flags().GetBool("raw")
	subs, _ := cmd.Flags().GetBool("subs")

	c := colly.NewCollector(
		colly.Async(true),
		colly.MaxDepth(maxDepth),
		colly.IgnoreRobotsTxt(),
	)

	// Setup http client
	client := &http.Client{}

	// Set proxy
	proxy, _ := cmd.Flags().GetString("proxy")
	if proxy != "" {
		Logger.Infof("Proxy: %s", proxy)
		pU, err := url.Parse(proxy)
		if err != nil {
			Logger.Error("Failed to set proxy")
		} else {
			DefaultHTTPTransport.Proxy = http.ProxyURL(pU)
		}
	}

	// Set request timeout
	timeout, _ := cmd.Flags().GetInt("timeout")
	if timeout == 0 {
		Logger.Info("Your input timeout is 0. Gospider will set it to 10 seconds")
		client.Timeout = 10 * time.Second
	} else {
		client.Timeout = time.Duration(timeout) * time.Second
	}

	// Disable redirect
	noRedirect, _ := cmd.Flags().GetBool("no-redirect")
	if noRedirect {
		client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
			nextLocation := req.Response.Header.Get("Location")
			Logger.Debugf("Found Redirect: %s", nextLocation)
			// Allow in redirect from http to https or in same hostname
			// We just check contain hostname or not because we set URLFilter in main collector so if
			// the URL is https://otherdomain.com/?url=maindomain.com, it will reject it
			if strings.Contains(nextLocation, site.Hostname()) {
				Logger.Infof("Redirecting to: %s", nextLocation)
				return nil
			}
			return http.ErrUseLastResponse
		}
	}

	// Set client transport
	client.Transport = DefaultHTTPTransport
	c.SetClient(client)

	// Get headers here to overwrite if "burp" flag used
	burpFile, _ := cmd.Flags().GetString("burp")
	if burpFile != "" {
		bF, err := os.Open(burpFile)
		if err != nil {
			Logger.Errorf("Failed to open Burp File: %s", err)
		} else {
			rd := bufio.NewReader(bF)
			req, err := http.ReadRequest(rd)
			if err != nil {
				Logger.Errorf("Failed to Parse Raw Request in %s: %s", burpFile, err)
			} else {
				// Set cookie
				c.OnRequest(func(r *colly.Request) {
					r.Headers.Set("Cookie", GetRawCookie(req.Cookies()))
				})

				// Set headers
				c.OnRequest(func(r *colly.Request) {
					for k, v := range req.Header {
						r.Headers.Set(strings.TrimSpace(k), strings.TrimSpace(v[0]))
					}
				})

			}
		}
	}

	// Set cookies
	cookie, _ := cmd.Flags().GetString("cookie")
	if cookie != "" && burpFile == "" {
		c.OnRequest(func(r *colly.Request) {
			r.Headers.Set("Cookie", cookie)
		})
	}

	// Set headers
	headers, _ := cmd.Flags().GetStringArray("header")
	if burpFile == "" {
		for _, h := range headers {
			headerArgs := strings.SplitN(h, ":", 2)
			headerKey := strings.TrimSpace(headerArgs[0])
			headerValue := strings.TrimSpace(headerArgs[1])
			c.OnRequest(func(r *colly.Request) {
				r.Headers.Set(headerKey, headerValue)
			})
		}
	}

	// Set User-Agent
	randomUA, _ := cmd.Flags().GetString("user-agent")
	switch ua := strings.ToLower(randomUA); {
	case ua == "mobi":
		extensions.RandomMobileUserAgent(c)
	case ua == "web":
		extensions.RandomUserAgent(c)
	default:
		c.UserAgent = ua
	}

	// Set referer
	extensions.Referer(c)

	// Init Output
	var output *Output
	outputFolder, _ := cmd.Flags().GetString("output")
	if outputFolder != "" {
		filename := strings.ReplaceAll(site.Hostname(), ".", "_")
		output = NewOutput(outputFolder, filename)
	}

	// Init Length Filter
	filterLength_slice := []int{}
	filterLength, _ := cmd.Flags().GetString("filter-length")

	if filterLength != "" {

		lengthArgs := strings.Split(filterLength, ",")
		for i := 0; i < len(lengthArgs); i++ {
			if i, err := strconv.Atoi(lengthArgs[i]); err == nil {
				filterLength_slice = append(filterLength_slice, i)
			}
		}
	}

	// Set url whitelist regex
	reg := ""
	if subs {
		reg = site.Hostname()
	} else {
		reg = "(?:https|http)://" + site.Hostname()
	}

	sRegex := regexp.MustCompile(reg)
	c.URLFilters = append(c.URLFilters, sRegex)

	// Set Limit Rule
	err := c.Limit(&colly.LimitRule{
		DomainGlob:  "*",
		Parallelism: concurrent,
		Delay:       time.Duration(delay) * time.Second,
		RandomDelay: time.Duration(randomDelay) * time.Second,
	})
	if err != nil {
		Logger.Errorf("Failed to set Limit Rule: %s", err)
		os.Exit(1)
	}

	// GoSpider default disallowed regex
	disallowedRegex := `(?i)\.(png|apng|bmp|gif|ico|cur|jpg|jpeg|jfif|pjp|pjpeg|svg|tif|tiff|webp|xbm|3gp|aac|flac|mpg|mpeg|mp3|mp4|m4a|m4v|m4p|oga|ogg|ogv|mov|wav|webm|eot|woff|woff2|ttf|otf|css)(?:\?|#|$)`
	c.DisallowedURLFilters = append(c.DisallowedURLFilters, regexp.MustCompile(disallowedRegex))

	// Set optional blacklist url regex
	blacklists, _ := cmd.Flags().GetString("blacklist")
	if blacklists != "" {
		c.DisallowedURLFilters = append(c.DisallowedURLFilters, regexp.MustCompile(blacklists))
	}

	// Set optional whitelist url regex
	whiteLists, _ := cmd.Flags().GetString("whitelist")
	if whiteLists != "" {
		c.URLFilters = make([]*regexp.Regexp, 0)
		c.URLFilters = append(c.URLFilters, regexp.MustCompile(whiteLists))
	}

	whiteListDomain, _ := cmd.Flags().GetString("whitelist-domain")
	if whiteListDomain != "" {
		c.URLFilters = make([]*regexp.Regexp, 0)
		c.URLFilters = append(c.URLFilters, regexp.MustCompile("http(s)?://"+whiteListDomain))
	}

	linkFinderCollector := c.Clone()
	// Try to request as much as Javascript source and don't care about domain.
	// The result of link finder will be send to Link Finder Collector to check is it working or not.
	linkFinderCollector.URLFilters = nil
	if whiteLists != "" {
		linkFinderCollector.URLFilters = append(linkFinderCollector.URLFilters, regexp.MustCompile(whiteLists))
	}
	if whiteListDomain != "" {
		linkFinderCollector.URLFilters = append(linkFinderCollector.URLFilters, regexp.MustCompile("http(s)?://"+whiteListDomain))
	}

	crawler := &Crawler{
		cmd:                 cmd,
		C:                   c,
		LinkFinderCollector: linkFinderCollector,
		site:                site,
		Quiet:               quiet,
		Input:               site.String(),
		JsonOutput:          jsonOutput,
		length:              length,
		raw:                 raw,
		domain:              domain,
		Output:              output,
		urlSet:              stringset.NewStringFilter(),
		subSet:              stringset.NewStringFilter(),
		jsSet:               stringset.NewStringFilter(),
		formSet:             stringset.NewStringFilter(),
		awsSet:              stringset.NewStringFilter(),
		filterLength_slice:  filterLength_slice,
		otherSourceFn:       OtherSources,
	}
	crawler.trackCollector(c)
	crawler.trackCollector(linkFinderCollector)

	return crawler
}

// trackCollector registers the terminal-state hooks of a Collector against the
// Crawler-level WaitGroup. A request is counted via visitCollector *before* it
// is handed to colly, and released exactly once here:
//   - OnScraped fires only on the success path (fetch returns without calling
//     handleOnError in a way that aborts the response),
//   - OnError fires on the failure path.
//
// colly never invokes both for the same request (a hard error returns from
// fetch before handleOnScraped is reached), so every counted request is Done
// exactly once. Requests rejected up-front by colly (duplicate, filtered,
// max-depth) never reach the network and are released inside visitCollector.
func (crawler *Crawler) trackCollector(collector *colly.Collector) {
	collector.OnScraped(func(*colly.Response) {
		crawler.wg.Done()
	})
	collector.OnError(func(*colly.Response, error) {
		crawler.wg.Done()
	})
}

// visitCollector enqueues a request while keeping the Crawler WaitGroup
// balanced. The counter is raised before Visit so that the work is already
// visible to Wait even if the request starts and finishes immediately. When
// colly rejects the URL synchronously (duplicate, disallowed filter, depth
// limit, parse error ...) no terminal callback will run, so the counter is
// released here instead.
func (crawler *Crawler) visitCollector(collector *colly.Collector, u string) error {
	crawler.wg.Add(1)
	if err := collector.Visit(u); err != nil {
		crawler.wg.Done()
		return err
	}
	return nil
}

// Visit enqueues a URL on the main Collector under full lifetime tracking.
func (crawler *Crawler) Visit(u string) error {
	return crawler.visitCollector(crawler.C, u)
}

// VisitLinkFinder enqueues a URL on the Link Finder Collector under full
// lifetime tracking.
func (crawler *Crawler) VisitLinkFinder(u string) error {
	return crawler.visitCollector(crawler.LinkFinderCollector, u)
}

// GoSource runs an entry-point source under lifetime tracking. Every discovery
// source (the main Start goroutine, robots.txt, sitemap.xml and third-party
// sources) is wrapped this way, so Wait cannot return while a source is still
// feeding URLs into the Collectors. The function returns immediately and the
// bookkeeping is released when fn returns.
func (crawler *Crawler) GoSource(fn func()) {
	crawler.wg.Add(1)
	go func() {
		defer crawler.wg.Done()
		fn()
	}()
}

// Wait blocks until every entry-point source has finished and every request of
// both Collectors - including all work derived recursively from their
// callbacks - has settled. It then closes the output file exactly once. After
// Wait returns, no tracked goroutine is alive and no further request or output
// is possible through the Crawler.
func (crawler *Crawler) Wait() {
	crawler.wg.Wait()
	crawler.closeOnce.Do(func() {
		if crawler.Output != nil {
			crawler.Output.Close()
		}
	})
}

// Run is the waitable entry point of a crawling job. It starts the main crawl
// plus the optional discovery sources (sitemap, robots, third-party sources),
// each tracked as an independent entry, and blocks until all of them and every
// request they transitively produce have completed. Start itself is unchanged
// and can still be called standalone when callers manage tracking themselves.
func (crawler *Crawler) Run(linkfinder, sitemap, robots, otherSource, includeSubs, includeOtherSourceResult bool) {
	crawler.doneOnce.Do(func() {
		crawler.GoSource(func() {
			crawler.Start(linkfinder)
		})

		if sitemap {
			crawler.GoSource(func() {
				ParseSiteMap(crawler.site, crawler)
			})
		}

		if robots {
			crawler.GoSource(func() {
				ParseRobots(crawler.site, crawler)
			})
		}

		if otherSource {
			crawler.GoSource(func() {
				crawler.runOtherSources(includeSubs, includeOtherSourceResult)
			})
		}
	})
	crawler.Wait()
}

// runOtherSources fetches third-party URLs through the injectable provider,
// emits the optional [other-sources] results and enqueues every URL into the
// main Collector under lifetime tracking.
func (crawler *Crawler) runOtherSources(includeSubs, includeOtherSourceResult bool) {
	urls := crawler.otherSourceFn(crawler.site.Hostname(), includeSubs)
	for _, u := range urls {
		u = strings.TrimSpace(u)
		if len(u) == 0 {
			continue
		}

		outputFormat := fmt.Sprintf("[other-sources] - %s", u)
		if includeOtherSourceResult {
			if crawler.JsonOutput {
				sout := SpiderOutput{
					Input:      crawler.Input,
					Source:     "other-sources",
					OutputType: "url",
					Output:     u,
				}
				if data, err := jsoniter.MarshalToString(sout); err == nil {
					outputFormat = data
				}
			} else if crawler.Quiet {
				outputFormat = u
			}
			fmt.Println(outputFormat)

			if crawler.Output != nil {
				crawler.Output.WriteToFile(outputFormat)
			}
		}

		_ = crawler.Visit(u)
	}
}

func (crawler *Crawler) feedLinkfinder(jsFileUrl string, OutputType string, source string) {

	if !crawler.jsSet.Duplicate(jsFileUrl) {
		outputFormat := fmt.Sprintf("[%s] - %s", OutputType, jsFileUrl)

		if crawler.JsonOutput {
			sout := SpiderOutput{
				Input:      crawler.Input,
				Source:     source,
				OutputType: OutputType,
				Output:     jsFileUrl,
			}
			if data, err := jsoniter.MarshalToString(sout); err == nil {
				outputFormat = data
				fmt.Println(outputFormat)
			}

		} else if !crawler.Quiet {
			fmt.Println(outputFormat)
		}

		if crawler.Output != nil {
			crawler.Output.WriteToFile(outputFormat)
		}

		// If JS file is minimal format. Try to find original format
		if strings.Contains(jsFileUrl, ".min.js") {
			originalJS := strings.ReplaceAll(jsFileUrl, ".min.js", ".js")
			_ = crawler.VisitLinkFinder(originalJS)
		}

		// Send Javascript to Link Finder Collector
		_ = crawler.VisitLinkFinder(jsFileUrl)

	}
}

func (crawler *Crawler) Start(linkfinder bool) {
	// Setup Link Finder
	if linkfinder {
		crawler.setupLinkFinder()
	}

	// Handle url
	crawler.C.OnHTML("[href]", func(e *colly.HTMLElement) {
		urlString := e.Request.AbsoluteURL(e.Attr("href"))
		urlString = FixUrl(crawler.site, urlString)
		if urlString == "" {
			return
		}
		if !crawler.urlSet.Duplicate(urlString) {
			outputFormat := fmt.Sprintf("[href] - %s", urlString)
			if crawler.JsonOutput {
				sout := SpiderOutput{
					Input:      crawler.Input,
					Source:     "body",
					OutputType: "form",
					Output:     urlString,
				}
				if data, err := jsoniter.MarshalToString(sout); err == nil {
					outputFormat = data
					fmt.Println(outputFormat)
				}
			} else if !crawler.Quiet {
				fmt.Println(outputFormat)
			}
			if crawler.Output != nil {
				crawler.Output.WriteToFile(outputFormat)
			}
			_ = crawler.Visit(urlString)
		}
	})

	// Handle form
	crawler.C.OnHTML("form[action]", func(e *colly.HTMLElement) {
		formUrl := e.Request.URL.String()
		if !crawler.formSet.Duplicate(formUrl) {
			outputFormat := fmt.Sprintf("[form] - %s", formUrl)
			if crawler.JsonOutput {
				sout := SpiderOutput{
					Input:      crawler.Input,
					Source:     "body",
					OutputType: "form",
					Output:     formUrl,
				}
				if data, err := jsoniter.MarshalToString(sout); err == nil {
					outputFormat = data
					fmt.Println(outputFormat)
				}
			} else if !crawler.Quiet {
				fmt.Println(outputFormat)
			}
			if crawler.Output != nil {
				crawler.Output.WriteToFile(outputFormat)
			}

		}
	})

	// Find Upload Form
	uploadFormSet := stringset.NewStringFilter()
	crawler.C.OnHTML(`input[type="file"]`, func(e *colly.HTMLElement) {
		uploadUrl := e.Request.URL.String()
		if !uploadFormSet.Duplicate(uploadUrl) {
			outputFormat := fmt.Sprintf("[upload-form] - %s", uploadUrl)
			if crawler.JsonOutput {
				sout := SpiderOutput{
					Input:      crawler.Input,
					Source:     "body",
					OutputType: "upload-form",
					Output:     uploadUrl,
				}
				if data, err := jsoniter.MarshalToString(sout); err == nil {
					outputFormat = data
					fmt.Println(outputFormat)
				}
			} else if !crawler.Quiet {
				fmt.Println(outputFormat)
			}
			if crawler.Output != nil {
				crawler.Output.WriteToFile(outputFormat)
			}
		}

	})

	// Handle js files
	crawler.C.OnHTML("[src]", func(e *colly.HTMLElement) {
		jsFileUrl := e.Request.AbsoluteURL(e.Attr("src"))
		jsFileUrl = FixUrl(crawler.site, jsFileUrl)
		if jsFileUrl == "" {
			return
		}

		fileExt := GetExtType(jsFileUrl)
		if fileExt == ".js" || fileExt == ".xml" || fileExt == ".json" {
			crawler.feedLinkfinder(jsFileUrl, "javascript", "body")
		}
	})

	crawler.C.OnResponse(func(response *colly.Response) {
		respStr := DecodeChars(string(response.Body))

		if len(crawler.filterLength_slice) == 0 || !contains(crawler.filterLength_slice, len(respStr)) {

			// Verify which link is working
			u := response.Request.URL.String()
			outputFormat := fmt.Sprintf("[url] - [code-%d] - %s", response.StatusCode, u)

			if crawler.length {
				outputFormat = fmt.Sprintf("[url] - [code-%d] - [len_%d] - %s", response.StatusCode, len(respStr), u)
			}

			if crawler.JsonOutput {
				sout := SpiderOutput{
					Input:      crawler.Input,
					Source:     "body",
					OutputType: "url",
					StatusCode: response.StatusCode,
					Output:     u,
					Length:     strings.Count(respStr, "\n"),
				}
				if data, err := jsoniter.MarshalToString(sout); err == nil {
					outputFormat = data
				}
			} else if crawler.Quiet {
				outputFormat = u
			}
			fmt.Println(outputFormat)
			if crawler.Output != nil {
				crawler.Output.WriteToFile(outputFormat)
			}
			if InScope(response.Request.URL, crawler.C.URLFilters) {
				crawler.findSubdomains(respStr)
				crawler.findAWSS3(respStr)
			}

			if crawler.raw {
				outputFormat := fmt.Sprintf("[Raw] - \n%s\n", respStr) //PRINTCLEAN RAW for link visited only
				if !crawler.Quiet {
					fmt.Println(outputFormat)
				}
				if crawler.Output != nil {
					crawler.Output.WriteToFile(outputFormat)
				}
			}
		}
	})

	crawler.C.OnError(func(response *colly.Response, err error) {
		Logger.Debugf("Error request: %s - Status code: %v - Error: %s", response.Request.URL.String(), response.StatusCode, err)
		/*
			1xx Informational
			2xx Success
			3xx Redirection
			4xx Client Error
			5xx Server Error
		*/
		if response.StatusCode == 404 || response.StatusCode == 429 || response.StatusCode < 100 || response.StatusCode >= 500 {
			return
		}

		u := response.Request.URL.String()
		outputFormat := fmt.Sprintf("[url] - [code-%d] - %s", response.StatusCode, u)

		if crawler.JsonOutput {
			sout := SpiderOutput{
				Input:      crawler.Input,
				Source:     "body",
				OutputType: "url",
				StatusCode: response.StatusCode,
				Output:     u,
				Length:     strings.Count(DecodeChars(string(response.Body)), "\n"),
			}
			if data, err := jsoniter.MarshalToString(sout); err == nil {
				outputFormat = data
				fmt.Println(outputFormat)
			}
		} else if crawler.Quiet {
			fmt.Println(u)
		} else {
			fmt.Println(outputFormat)
		}

		if crawler.Output != nil {
			crawler.Output.WriteToFile(outputFormat)
		}
	})

	err := crawler.Visit(crawler.site.String())
	if err != nil {
		Logger.Errorf("Failed to start %s: %s", crawler.site.String(), err)
	}
}

// Find subdomains from response
func (crawler *Crawler) findSubdomains(resp string) {
	subs := GetSubdomains(resp, crawler.domain)
	for _, sub := range subs {
		if !crawler.subSet.Duplicate(sub) {
			outputFormat := fmt.Sprintf("[subdomains] - %s", sub)

			if crawler.JsonOutput {
				sout := SpiderOutput{
					Input:      crawler.Input,
					Source:     "body",
					OutputType: "subdomain",
					Output:     sub,
				}
				if data, err := jsoniter.MarshalToString(sout); err == nil {
					outputFormat = data
				}
				fmt.Println(outputFormat)
			} else if !crawler.Quiet {
				outputFormat = fmt.Sprintf("[subdomains] - http://%s", sub)
				fmt.Println(outputFormat)
				outputFormat = fmt.Sprintf("[subdomains] - https://%s", sub)
				fmt.Println(outputFormat)
			}
			if crawler.Output != nil {
				crawler.Output.WriteToFile(outputFormat)
			}
		}
	}
}

// Find AWS S3 from response
func (crawler *Crawler) findAWSS3(resp string) {
	aws := GetAWSS3(resp)
	for _, e := range aws {
		if !crawler.awsSet.Duplicate(e) {
			outputFormat := fmt.Sprintf("[aws-s3] - %s", e)
			if crawler.JsonOutput {
				sout := SpiderOutput{
					Input:      crawler.Input,
					Source:     "body",
					OutputType: "aws",
					Output:     e,
				}
				if data, err := jsoniter.MarshalToString(sout); err == nil {
					outputFormat = data
				}
			}
			fmt.Println(outputFormat)
			if crawler.Output != nil {
				crawler.Output.WriteToFile(outputFormat)
			}
		}
	}
}

// Setup link finder
func (crawler *Crawler) setupLinkFinder() {
	crawler.LinkFinderCollector.OnResponse(func(response *colly.Response) {
		if response.StatusCode == 404 || response.StatusCode == 429 || response.StatusCode < 100 {
			return
		}

		respStr := string(response.Body)

		if len(crawler.filterLength_slice) == 0 || !contains(crawler.filterLength_slice, len(respStr)) {

			// Verify which link is working
			u := response.Request.URL.String()
			outputFormat := fmt.Sprintf("[url] - [code-%d] - %s", response.StatusCode, u)

			if crawler.length {
				outputFormat = fmt.Sprintf("[url] - [code-%d] - [len_%d] - %s", response.StatusCode, len(respStr), u)
			}

			if crawler.JsonOutput {
				sout := SpiderOutput{
					Input:      crawler.Input,
					Source:     "body",
					OutputType: "url",
					StatusCode: response.StatusCode,
					Output:     u,
					Length:     strings.Count(respStr, "\n"),
				}
				if data, err := jsoniter.MarshalToString(sout); err == nil {
					outputFormat = data
				}
			} else if crawler.Quiet {
				outputFormat = u
			}
			fmt.Println(outputFormat)

			if crawler.Output != nil {
				crawler.Output.WriteToFile(outputFormat)
			}

			if InScope(response.Request.URL, crawler.C.URLFilters) {

				crawler.findSubdomains(respStr)
				crawler.findAWSS3(respStr)

				paths, err := LinkFinder(respStr)
				if err != nil {
					Logger.Error(err)
					return
				}

				currentPathURL, err := url.Parse(u)
				currentPathURLerr := false
				if err != nil {
					currentPathURLerr = true
				}

				for _, relPath := range paths {
					var outputFormat string
					// JS Regex Result
					if crawler.JsonOutput {
						sout := SpiderOutput{
							Input:      crawler.Input,
							Source:     response.Request.URL.String(),
							OutputType: "linkfinder",
							Output:     relPath,
						}
						if data, err := jsoniter.MarshalToString(sout); err == nil {
							outputFormat = data
						}
					} else if !crawler.Quiet {
						outputFormat = fmt.Sprintf("[linkfinder] - [from: %s] - %s", response.Request.URL.String(), relPath)
					}
					fmt.Println(outputFormat)

					if crawler.Output != nil {
						crawler.Output.WriteToFile(outputFormat)
					}
					rebuildURL := ""
					if !currentPathURLerr {
						rebuildURL = FixUrl(currentPathURL, relPath)
					} else {
						rebuildURL = FixUrl(crawler.site, relPath)
					}

					if rebuildURL == "" {
						continue
					}

					// Try to request JS path
					// Try to generate URLs with main site
					fileExt := GetExtType(rebuildURL)
					if fileExt == ".js" || fileExt == ".xml" || fileExt == ".json" || fileExt == ".map" {
						crawler.feedLinkfinder(rebuildURL, "linkfinder", "javascript")
					} else if !crawler.urlSet.Duplicate(rebuildURL) {

						if crawler.JsonOutput {
							sout := SpiderOutput{
								Input:      crawler.Input,
								Source:     response.Request.URL.String(),
								OutputType: "linkfinder",
								Output:     rebuildURL,
							}
							if data, err := jsoniter.MarshalToString(sout); err == nil {
								outputFormat = data
							}
						} else if !crawler.Quiet {
							outputFormat = fmt.Sprintf("[linkfinder] - %s", rebuildURL)
						}

						fmt.Println(outputFormat)

						if crawler.Output != nil {
							crawler.Output.WriteToFile(outputFormat)
						}
						_ = crawler.Visit(rebuildURL)
					}

					// Try to generate URLs with the site where Javascript file host in (must be in main or sub domain)

					urlWithJSHostIn := FixUrl(crawler.site, relPath)
					if urlWithJSHostIn != "" {
						fileExt := GetExtType(urlWithJSHostIn)
						if fileExt == ".js" || fileExt == ".xml" || fileExt == ".json" || fileExt == ".map" {
							crawler.feedLinkfinder(urlWithJSHostIn, "linkfinder", "javascript")
						} else {
							if crawler.urlSet.Duplicate(urlWithJSHostIn) {
								continue
							} else {

								if crawler.JsonOutput {
									sout := SpiderOutput{
										Input:      crawler.Input,
										Source:     response.Request.URL.String(),
										OutputType: "linkfinder",
										Output:     urlWithJSHostIn,
									}
									if data, err := jsoniter.MarshalToString(sout); err == nil {
										outputFormat = data
									}
								} else if !crawler.Quiet {
									outputFormat = fmt.Sprintf("[linkfinder] - %s", urlWithJSHostIn)
								}
								fmt.Println(outputFormat)

								if crawler.Output != nil {
									crawler.Output.WriteToFile(outputFormat)
								}
								_ = crawler.Visit(urlWithJSHostIn) //not print care for lost link
							}
						}

					}

				}

				if crawler.raw {

					outputFormat := fmt.Sprintf("[Raw] - \n%s\n", respStr) //PRINTCLEAN RAW for link visited only
					if !crawler.Quiet {
						fmt.Println(outputFormat)
					}

					if crawler.Output != nil {
						crawler.Output.WriteToFile(outputFormat)
					}
				}
			}
		}
	})
}
