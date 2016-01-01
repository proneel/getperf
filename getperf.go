package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"math"
	"math/rand"
	"net/http"
	"os"
	"sort"
	"time"

	"github.com/golang/glog"
	"github.com/pivotal-golang/bytefmt"
)

// structure storing information on response of a URL GET
type HitInfo struct {
	url        string // URL that was hit
	status     int    // 200 OK, 400 etc
	contentlen int64  // length of body
	timemsec   int    // time taken for the call in msecs
}

// Load all urls from a file into a slice
func readfile(filepath string) []string {
	file, err := os.Open(filepath)
	if err != nil {
		fmt.Println(err)
		os.Exit(2)
	}
	defer file.Close()

	urls := make([]string, 0) // start with an empty slice, will grow as we append (not best in terms of performance, but this is done only once)
	scanner := bufio.NewScanner(file)
	for scanner.Scan() { // read each line (URL)
		urls = append(urls, scanner.Text()) // copy it into the slice
	}

	if err := scanner.Err(); err != nil {
		fmt.Println(err)
		os.Exit(2)
	}

	fmt.Printf("Number of URLs read = %d\n", len(urls))

	return urls
}

// hit a random url from the slice and send the results of the hit on the channel ch
func hitrandomurl(urls []string, ch chan HitInfo) {
	index := rand.Int31n(int32(len(urls)))
	t1 := time.Now() // record when we started

	resp, err := http.Get(urls[index]) // hit the url
	if err != nil {
		glog.V(0).Infof("url %s gave error = %v\n", err)
		ch <- HitInfo{urls[index], -1, 0, 0}
		return
	}

	defer resp.Body.Close()

	io.Copy(ioutil.Discard, resp.Body) // discard the body
	diff := time.Since(t1)             // calculate how long it took

	// write this information to the channel
	ch <- HitInfo{urls[index], resp.StatusCode, resp.ContentLength, int(diff / 1000000)}
}

// simple function to calculate median and standard deviation on response times
func stats(values []int) (float64, int, float64) {
	// sort the array to get the median
	sort.Ints(values)
	median := values[len(values)/2]

	// now calculate the mean by sum(values)/count(values)
	sum := int64(0)
	for i := 0; i < len(values); i++ {
		sum += int64(values[i])
	}

	var mean float64 = float64(sum / int64(len(values)))

	// calculate sum of squares of diff from mean
	sumsqdiff := float64(0)
	for i := 0; i < len(values); i++ {
		sumsqdiff += (mean - float64(values[i])) * (mean - float64(values[i]))
	}
	// stddev is sqrt of mean of above
	sd := math.Sqrt(sumsqdiff / float64(len(values)))

	return mean, median, sd
}

// main function that loads the url files and fires off goroutines based on number of threads
// and keeps tab on the stats in the main thread
func main() {
	var urlfile string
	var runfor int = 60
	var threads int = 1

	// command line flags
	flag.StringVar(&urlfile, "urlfile", "", "path to file with list of urls")
	flag.IntVar(&runfor, "runfor", 60, "how long to run the program for (secs)")
	flag.IntVar(&threads, "threads", 1, "number of parallel threads")

	flag.Parse()

	if urlfile == "" {
		os.Stderr.WriteString("Missing required parameter urlfile\n")
		os.Exit(1)
	}

	// list of URLs to GET
	urls := readfile(urlfile)

	// seed it so the random number generation is actually somewhat random
	rand.Seed(int64(time.Now().Second()))

	// make a channel for the threads to send hit info back to the main thread
	ch := make(chan HitInfo, threads*2) // buffer size equal to twice number of parallel goroutines

	// go routine which will make hits forever
	forever := func() {
		for {
			hitrandomurl(urls, ch)
		}
	}

	// start required number of go routines in parallel
	for i := 0; i < threads; i++ {
		go forever()
	}

	var count200 int64 = 0  // count of GETs that resulted in 200 OK
	var countfail int64 = 0 // count of GETs that did not return 200 OK
	var timeMsec int64 = 0  // sum of time taken for all successful requests in MSecs
	var bytes uint64 = 0    // sum of bytes read in all successful requests in MSecs

	// give an update to the user at minimum every minute, at maximum every 5 secs, but otherwise at each 10% of time intervals
	ticksInterval := int64(math.Max(math.Min(float64(runfor/10.0), 60.0), 5.0))

	// create a ticker to wake us up to print current stats
	ticksC := time.NewTicker(time.Duration(ticksInterval) * time.Second).C
	// create a timer to wake us up when we should end
	endC := time.NewTimer(time.Duration(runfor) * time.Second).C

	// store each time taken value in a slice so we can do stats on it
	msecSlice := make([]int, 0)

forloop:
	// in a continuous loop, check if the end timer has fired, if it has print final stats and exit the loop
	// if the interim stats ticker has fired, print interim stats
	// if we received a hit info from a goroutine on the channel, add that to the known dataset
	for {
		select {
		case <-endC: // end timer fired
			_, median, sd := stats(msecSlice)
			fmt.Printf("Final result:\n num200OK=%v numFail=%v bytes=%v avgTimeMsec=%v, medianTimeMsec=%v, stddevTimeMsec=%.2f\n", count200, countfail, bytefmt.ByteSize(bytes), timeMsec/count200, median, sd)
			break forloop // end the program
		case <-ticksC: // time for us to print stats until now
			fmt.Println(count200, countfail, bytes, timeMsec/count200)
		case info := <-ch: // we got hit info from a thread
			glog.V(2).Info(info)
			if info.status == 200 {
				count200++
				msecSlice = append(msecSlice, info.timemsec)
				timeMsec += int64(info.timemsec)
				bytes += uint64(info.contentlen)
			} else {
				countfail++
			}
		}
	}
}
