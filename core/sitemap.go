package core

import (
	"fmt"
	"net/url"

	jsoniter "github.com/json-iterator/go"

	sitemap "github.com/oxffaa/gopher-parse-sitemap"
)

// ParseSiteMap probes the well-known sitemap locations and enqueues every entry
// into the Crawler's main Collector through its lifetime-tracked Visit. It is
// meant to be run inside Crawler.GoSource so that the job's Wait cannot return
// while the sitemap discovery is still in progress.
func ParseSiteMap(site *url.URL, crawler *Crawler) {
	sitemapUrls := []string{"/sitemap.xml", "/sitemap_news.xml", "/sitemap_index.xml", "/sitemap-index.xml", "/sitemapindex.xml",
		"/sitemap-news.xml", "/post-sitemap.xml", "/page-sitemap.xml", "/portfolio-sitemap.xml", "/home_slider-sitemap.xml", "/category-sitemap.xml",
		"/author-sitemap.xml"}

	for _, path := range sitemapUrls {
		// Ignore error when that not valid sitemap.xml path
		Logger.Infof("Trying to find %s", site.String()+path)
		_ = sitemap.ParseFromSite(site.String()+path, func(entry sitemap.Entry) error {
			outputFormat := fmt.Sprintf("[sitemap] - %s", entry.GetLocation())

			if crawler.JsonOutput {
				sout := SpiderOutput{
					Input:      crawler.Input,
					Source:     "sitemap",
					OutputType: "url",
					Output:     entry.GetLocation(),
				}
				if data, err := jsoniter.MarshalToString(sout); err == nil {
					outputFormat = data
				}
			} else if crawler.Quiet {
				outputFormat = entry.GetLocation()
			}
			fmt.Println(outputFormat)
			if crawler.Output != nil {
				crawler.Output.WriteToFile(outputFormat)
			}
			_ = crawler.Visit(entry.GetLocation())
			return nil
		})
	}

}
