package crawl

import (
	"os"
	"os/signal"
	"path"
	"syscall"
	"time"

	"github.com/sirupsen/logrus"
)

// catchFinish is running in the background and detect when the crawl need to be terminated
// because it won't crawl anything more. This doesn't apply for Kafka-powered crawls.
func (crawl *Crawl) catchFinish() {
	for crawl.Crawled.Value() <= 0 {
		time.Sleep(1 * time.Second)
	}

	for {
		time.Sleep(time.Second * 5)
		if crawl.ActiveWorkers.Value() == 0 && crawl.Frontier.QueueCount.Value() == 0 && crawl.Finished.Get() == false && crawl.Crawled.Value() > 0 {
			logrus.Warning("No additional URL to archive, finishing")
			crawl.finish()
			os.Exit(0)
		}
	}
}

func (crawl *Crawl) finish() {
	crawl.Finished.Set(true)

	crawl.WorkerPool.Wait()
	logrus.Warning("All workers finished")

	if crawl.WARC {
		close(crawl.WARCWriter)
		<-crawl.WARCWriterFinish
		close(crawl.WARCWriterFinish)
		logrus.Warning("WARC writer closed")
	}

	crawl.Frontier.Queue.Close()
	logrus.Warning("Frontier queue closed")

	if crawl.Seencheck {
		crawl.Frontier.Seencheck.SeenDB.Close()
		logrus.Warning("Seencheck database closed")
	}

	logrus.Warning("Dumping hosts pool and frontier stats to " + path.Join(crawl.Frontier.JobPath, "frontier.gob"))
	crawl.Frontier.Save()

	close(crawl.Frontier.PullChan)
	close(crawl.Frontier.PushChan)

	logrus.Warning("Finished")
}

func (crawl *Crawl) setupCloseHandler() {
	c := make(chan os.Signal)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	<-c
	logrus.Warning("CTRL+C catched.. cleaning up and exiting.")
	signal.Stop(c)
	close(c)
	crawl.finish()
	os.Exit(0)
}
