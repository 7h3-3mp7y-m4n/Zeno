package crawl

import (
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/CorentinB/Zeno/internal/pkg/utils"

	"github.com/CorentinB/Zeno/internal/pkg/frontier"
	"github.com/PuerkitoBio/goquery"
	"github.com/sirupsen/logrus"
)

func (c *Crawl) executeGET(parentItem *frontier.Item, req *http.Request) (resp *http.Response, respPath string, err error) {
	var newItem *frontier.Item
	var newReq *http.Request
	var URL *url.URL

	// Check if the crawl is paused
	for c.Paused.Get() {
		time.Sleep(time.Second)
	}

	// Execute GET request
	if c.ClientProxied == nil || utils.StringContainsSliceElements(req.URL.Host, c.BypassProxy) {
		resp, err = c.Client.Do(req)
		if err != nil {
			return resp, respPath, err
		}
	} else {
		resp, err = c.ClientProxied.Do(req)
		if err != nil {
			return resp, respPath, err
		}
	}

	if c.Prometheus {
		c.PrometheusMetrics.DownloadedURI.Inc()
	}

	c.URIsPerSecond.Incr(1)
	c.Crawled.Incr(1)

	// If a redirection is catched, then we execute the redirection
	if isRedirection(resp.StatusCode) {
		if resp.Header.Get("location") == req.URL.String() || parentItem.Redirect >= c.MaxRedirect {
			return resp, respPath, nil
		}

		defer markTempFileDone(respPath)

		URL, err = url.Parse(resp.Header.Get("location"))
		if err != nil {
			return resp, respPath, err
		}

		newItem = frontier.NewItem(URL, parentItem, parentItem.Type, parentItem.Hop)
		newItem.Redirect = parentItem.Redirect + 1

		// Prepare GET request
		newReq, err = http.NewRequest("GET", URL.String(), nil)
		if err != nil {
			return resp, respPath, err
		}

		req.Header.Set("User-Agent", c.UserAgent)
		req.Header.Set("Referer", newItem.ParentItem.URL.String())

		resp, respPath, err = c.executeGET(newItem, newReq)
		if err != nil {
			return resp, respPath, err
		}
	}
	return resp, respPath, nil
}

func (c *Crawl) captureAsset(item *frontier.Item, cookies []*http.Cookie) error {
	var executionStart = time.Now()
	var resp *http.Response

	// If --seencheck is enabled, then we check if the URI is in the
	// seencheck DB before doing anything. If it is in it, we skip the item
	if c.Seencheck {
		hash := strconv.FormatUint(item.Hash, 10)
		found, _ := c.Frontier.Seencheck.IsSeen(hash)
		if found {
			return nil
		}
		c.Frontier.Seencheck.Seen(hash, item.Type)
	}

	// Prepare GET request
	req, err := http.NewRequest("GET", item.URL.String(), nil)
	if err != nil {
		return err
	}

	req.Header.Set("Referer", item.ParentItem.URL.String())

	// Apply cookies obtained from the original URL captured
	for i := range cookies {
		req.AddCookie(cookies[i])
	}

	resp, respPath, err := c.executeGET(item, req)
	if err != nil {
		markTempFileDone(respPath)
		return err
	}
	defer resp.Body.Close()
	defer markTempFileDone(respPath)

	c.logCrawlSuccess(executionStart, resp.StatusCode, item)

	return nil
}

// Capture capture the URL and return the outlinks
func (c *Crawl) Capture(item *frontier.Item) {
	var executionStart = time.Now()
	var resp *http.Response

	// Prepare GET request
	req, err := http.NewRequest("GET", item.URL.String(), nil)
	if err != nil {
		logWarning.WithFields(logrus.Fields{
			"error": err,
		}).Warning(item.URL.String())
		return
	}

	if item.Hop > 0 && len(item.ParentItem.URL.String()) > 0 {
		req.Header.Set("Referer", item.ParentItem.URL.String())
	}

	req.Header.Set("Authority", "www.tiktok.com")
	req.Header.Set("Sec-Ch-Ua", "\" Not A;Brand\";v=\"99\", \"Chromium\";v=\"99\", \"Microsoft Edge\";v=\"99\"")
	req.Header.Set("Sec-Ch-Ua-Mobile", "?0")
	req.Header.Set("Sec-Ch-Ua-Platform", "\"Linux\"")
	req.Header.Set("Dnt", "1")
	req.Header.Set("Upgrade-Insecure-Requests", "1")
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/99.0.4844.74 Safari/537.36 Edg/99.0.1150.52")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/webp,image/apng,*/*;q=0.8,application/signed-exchange;v=b3;q=0.9")
	req.Header.Set("Sec-Fetch-Site", "none")
	req.Header.Set("Sec-Fetch-Mode", "navigate")
	req.Header.Set("Sec-Fetch-User", "?1")
	req.Header.Set("Sec-Fetch-Dest", "document")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9,fr;q=0.8")

	// execute request
	resp, respPath, err := c.executeGET(item, req)
	if err != nil {
		logWarning.WithFields(logrus.Fields{
			"error": err,
		}).Warning(item.URL.String())
		markTempFileDone(respPath)
		return
	}
	defer resp.Body.Close()
	defer markTempFileDone(respPath)

	c.logCrawlSuccess(executionStart, resp.StatusCode, item)

	// If the response isn't a text/*, we do not scrape it, and we delete the
	// temporary file if it exists
	if strings.Contains(resp.Header.Get("Content-Type"), "text/") == false {
		// enforce reading all data from the response
		io.Copy(io.Discard, resp.Body)
		return
	}

	// Store the base URL to turn relative links into absolute links later
	base, err := url.Parse(resp.Request.URL.String())
	if err != nil {
		logWarning.WithFields(logrus.Fields{
			"error": err,
		}).Warning(item.URL.String())
		return
	}

	// Turn the response into a doc that we will scrape
	var doc *goquery.Document
	if respPath != "" {
		file, err := os.Open(respPath)
		if err != nil {
			logWarning.WithFields(logrus.Fields{
				"error": err,
				"url":   item.URL.String(),
				"path":  respPath,
			}).Warning("Error opening temporary file for outlinks/assets extraction")
			return
		}

		doc, err = goquery.NewDocumentFromReader(file)
		if err != nil {
			logWarning.WithFields(logrus.Fields{
				"error": err,
				"url":   item.URL.String(),
				"path":  respPath,
			}).Warning("Error making goquery document from temporary file")
			return
		}
		_ = doc
		file.Close()
		markTempFileDone(respPath)
	} else {
		doc, err = goquery.NewDocumentFromResponse(resp)
		if err != nil {
			logWarning.WithFields(logrus.Fields{
				"error": err,
			}).Warning(item.URL.String())
			return
		}
		_ = doc
	}

	// Extract outlinks
	if item.Hop < c.MaxHops {
		outlinks, err := extractOutlinks(base, doc)
		if err != nil {
			logWarning.WithFields(logrus.Fields{
				"error": err,
			}).Warning(item.URL.String())
			return
		}
		go c.queueOutlinks(outlinks, item)
	}

	// Extract and capture assets
	assets, err := c.extractAssets(base, doc)
	if err != nil {
		logWarning.WithFields(logrus.Fields{
			"error": err,
		}).Warning(item.URL.String())
		return
	}

	c.Frontier.QueueCount.Incr(int64(len(assets)))
	for _, asset := range assets {
		c.Frontier.QueueCount.Incr(-1)

		// Just making sure we do not over archive
		if item.URL.String() == asset.String() {
			continue
		}

		newAsset := frontier.NewItem(&asset, item, "asset", item.Hop)
		err = c.captureAsset(newAsset, resp.Cookies())
		if err != nil {
			logWarning.WithFields(logrus.Fields{
				"error":          err,
				"queued":         c.Frontier.QueueCount.Value(),
				"crawled":        c.Crawled.Value(),
				"rate":           c.URIsPerSecond.Rate(),
				"active_workers": c.ActiveWorkers.Value(),
				"parent_hop":     item.Hop,
				"parent_url":     item.URL.String(),
				"type":           "asset",
			}).Warning(asset.String())
			continue
		}
	}
}

func markTempFileDone(path string) {
	if path != "" {
		os.Rename(path, path+".done")
	}
}
