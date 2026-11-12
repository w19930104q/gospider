package core

import (
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	jsoniter "github.com/json-iterator/go"
)

// ParseRobots fetches /robots.txt and enqueues every allowed path into the
// Crawler's main Collector through its lifetime-tracked Visit. It is meant to
// be run inside Crawler.GoSource so that the job's Wait cannot return while the
// robots discovery is still in progress.
func ParseRobots(site *url.URL, crawler *Crawler) {
	robotsURL := site.String() + "/robots.txt"

	resp, err := http.Get(robotsURL)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode == 200 {
		Logger.Infof("Found robots.txt: %s", robotsURL)
		body, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			return
		}
		lines := strings.Split(string(body), "\n")

		var re = regexp.MustCompile(".*llow: ")
		for _, line := range lines {
			if strings.Contains(line, "llow: ") {
				url := re.ReplaceAllString(line, "")
				url = FixUrl(site, url)
				if url == "" {
					continue
				}
				outputFormat := fmt.Sprintf("[robots] - %s", url)

				if crawler.JsonOutput {
					sout := SpiderOutput{
						Input:      crawler.Input,
						Source:     "robots",
						OutputType: "url",
						Output:     url,
					}
					if data, err := jsoniter.MarshalToString(sout); err == nil {
						outputFormat = data
					}
				} else if crawler.Quiet {
					outputFormat = url
				}
				fmt.Println(outputFormat)
				if crawler.Output != nil {
					crawler.Output.WriteToFile(outputFormat)
				}
				_ = crawler.Visit(url)
			}
		}

	}

}
