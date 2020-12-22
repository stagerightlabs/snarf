package main

import (
	"crypto/md5"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/user"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/mmcdole/gofeed"
)

func main() {
	var feed string
	var destination string
	start := time.Now()

	flag.StringVar(&feed, "f", "", "The rss feed to inspect")
	flag.StringVar(&destination, "d", "", "The destination directory")
	flag.Parse()

	if len(feed) == 0 {
		fmt.Println("No feed provided.")
		return
	}

	if len(destination) == 0 {
		destination = getUserConfigDirectory()
	}

	// Make sure the destination directory exists
	_, err := os.Stat(destination)
	if os.IsNotExist(err) {
		os.MkdirAll(destination, 0755)
	}

	// Read the contents of the file
	contents, err := readFeed(feed, destination)
	if err != nil {
		panic(err)
	}

	fmt.Println("Checking feed contents...")

	feedName := snakeCase(contents.Title)

	// Add jobs to the queue
	var jobs []Job
	for i := len(contents.Items) - 1; i >= 0; i-- {
		job := Job{
			ID:          i,
			Item:        contents.Items[i],
			Destination: destination + "/" + feedName,
		}
		jobs = append(jobs, job)
	}

	const NumberOfWorkers = 5
	var (
		wg         sync.WaitGroup
		jobChannel = make(chan Job)
	)
	wg.Add(NumberOfWorkers)

	// start the workers
	for i := 0; i < NumberOfWorkers; i++ {
		go worker(i, &wg, jobChannel)
	}

	// Send jobs to workers
	for _, job := range jobs {
		jobChannel <- job
	}
	close(jobChannel)
	wg.Wait()

	fmt.Printf("Took %s\n", time.Since(start))
}

func generateFeedHash(feed string) string {
	h := md5.New()
	io.WriteString(h, feed)

	return fmt.Sprintf("%x", h.Sum(nil))
}

// Read the contents of an rss file into memory. If the file
// has not been cached locally it will be downloaded.
func readFeed(feed string, destination string) (*gofeed.Feed, error) {
	feedsFolder := destination + "/feeds"
	filePath := feedsFolder + "/" + generateFeedHash(feed)

	// Make sure the "feeds" folder exists
	_, err := os.Stat(feedsFolder)
	if os.IsNotExist(err) {
		os.MkdirAll(feedsFolder, 0755)
	}

	fileInfo, err := os.Stat(filePath)
	if os.IsNotExist(err) {
		fmt.Println("Downloading feed contents: ", filePath)
		downloadFile(feed, filePath)
	}

	// Is the locally cached file older than seven days?
	currentTime := time.Now()
	diff := currentTime.Sub(fileInfo.ModTime())
	if diff.Minutes() > (60 * 24 * 7) {
		fmt.Println("Refreshing feed contents: ", filePath)
		downloadFile(feed, filePath)
	}

	file, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	fp := gofeed.NewParser()

	return fp.Parse(file)
}

// https://progolang.com/how-to-download-files-in-go/
func downloadFile(url string, filepath string) error {
	// create the file
	out, err := os.Create(filepath)
	if err != nil {
		return err
	}
	defer out.Close()

	// Get the data
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	// How can we determine if a download has not completed?

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return errors.New("Could not reach " + url)
	}

	// Write the body to file
	_, err = io.Copy(out, resp.Body)
	if err != nil {
		return err
	}

	return nil
}

// Get the user's "config" directory
func getUserConfigDirectory() string {
	usr, err := user.Current()
	if err != nil {
		return ""
	}

	return usr.HomeDir + "/.config/snarf"
}

// https://twinnation.org/articles/39/go-concurrency-goroutines-worker-pools-and-throttling-made-simple
func worker(id int, wg *sync.WaitGroup, jobChannel <-chan Job) {
	defer wg.Done()
	for job := range jobChannel {
		result := maybeDownloadItem(id, job)
		if result.Important {
			fmt.Printf("%s\n", result.Message)
		}
		if result.Downloaded {
			time.Sleep(5 * time.Second)
		}
	}
}

// Download the contents of an Item enclosure if it does not already
// exist in the destination folder.
func maybeDownloadItem(id int, job Job) JobResult {

	// If no enclosures are listed we will skip this item
	if len(job.Item.Enclosures) == 0 {
		return JobResult{
			Message:    "No file to download",
			Important:  false,
			Downloaded: false,
		}
	}

	enclosure := job.Item.Enclosures[0]

	// Extract the file extension from the download URL
	extension, err := fileExtensionFromURL(enclosure.URL)

	if err != nil {
		return JobResult{
			Message:    "No file to download",
			Important:  false,
			Downloaded: false,
		}
	}

	// Make sure our destination folder exists
	if !fileExists(job.Destination) {
		os.MkdirAll(job.Destination, 0755)
	}

	// Generate a path for our download destination
	fileName := snakeCase(job.Item.Title)
	path := job.Destination + "/" + fileName + extension

	if fileExists(path) {
		return JobResult{
			Message:    "Already Downloaded " + path,
			Important:  false,
			Downloaded: false,
		}
	}

	err = downloadFile(enclosure.URL, path)
	if err != nil {
		return JobResult{
			Message:    err.Error(),
			Important:  true,
			Downloaded: false,
		}
	}

	return JobResult{
		Message:    fmt.Sprintf("downloaded: %s", job.Item.Title),
		Important:  true,
		Downloaded: true,
	}
}

func fileExtensionFromURL(href string) (string, error) {
	// Parse the URL
	u, err := url.Parse(href)
	if err != nil {
		return "", err
	}
	// Remove the query parameters
	u.RawQuery = ""

	// Extract the file name from the URL
	filename := path.Base(u.String())

	// Find the location of the "."
	pivot := strings.Index(filename, ".")

	// Return the file extension as a string
	return filename[pivot:], nil
}

func snakeCase(name string) string {
	name = strings.ToLower(name)
	name = strings.Replace(name, " ", "_", -1)
	name = strings.Replace(name, ":", "", -1)

	return name
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return !os.IsNotExist(err)
}

type Job struct {
	ID          int
	Item        *gofeed.Item
	Destination string
}

type JobResult struct {
	Message    string
	Important  bool
	Downloaded bool
}
