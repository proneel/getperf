package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"math"
	"math/rand"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/golang/glog"
	"github.com/phayes/freeport"
	"github.com/pivotal-golang/bytefmt"
)

// structure storing information on response of a URL GET
type HitInfo struct {
	Url        string // URL that was hit
	Status     int    // 200 OK, 400 etc
	Contentlen int64  // length of body
	Timemsec   int    // time taken for the call in msecs
}

// structure storing information about a request from a Manager to an Agent to process a perf request
type ProcessRequest struct {
	ManagerUrl  string   // URL the Agent will callback on to report results
	AgentNumber int      // a unique ID of the Agent
	Urls        []string // the URLs to hit in this 'batch'
}

// Load all urls from a file into a slice and return the slice
// TODO: deal with large files where all URLs are not read in
func readfile(filepath string) []string {
	// open the file of URLs
	file, err := os.Open(filepath)
	if err != nil {
		fmt.Println(err)
		os.Exit(2)
	}
	defer file.Close()

	// start with an empty slice, will grow as we append (not best in terms of performance, but this is done only once)
	urls := make([]string, 0)
	scanner := bufio.NewScanner(file)
	for scanner.Scan() { // read each line (URL)
		urls = append(urls, scanner.Text()) // copy it into the slice
	}

	// there should be no scanning errors
	if err := scanner.Err(); err != nil {
		fmt.Println(err)
		os.Exit(2)
	}

	fmt.Printf("Number of URLs read = %d\n", len(urls))

	return urls
}

// hit the url and send the results of the hit on the channel ch
func hitUrlAndReport(url string, ch chan HitInfo) {
	t1 := time.Now() // record when we started

	resp, err := http.Get(url) // hit the url
	if err != nil {
		glog.V(0).Infof("url %s gave error = %v\n", url, err)
		ch <- HitInfo{url, -1, 0, 0} // note error on the channel
		return
	}

	defer resp.Body.Close()

	io.Copy(ioutil.Discard, resp.Body) // discard the body
	diff := time.Since(t1)             // calculate how long it took

	// write this information to the channel
	ch <- HitInfo{url, resp.StatusCode, resp.ContentLength, int(diff / 1000000)}
}

// simple function to calculate median and standard deviation on response times
// returns mean, median and standard deviation on the set of values passed in
func stats(values []int) (float64, int, float64) {
	if len(values) == 0 {
		return 0, 0, 0 // no sense in stats for an empty slice
	}

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

// An agent thread (go routine) which hits URLs as they arrive on in-channel inch and reports performance results HitInfo on the outbound channel outch
func agentSubProcess(id int, inch chan string, outch chan HitInfo) {
	defer func() {
		if r := recover(); r != nil {
			os.Stderr.WriteString("agentSubProcess recovered from a panic, probably because the channel outch was closed\n")
		}
	}()

	// keep processing urls coming on inch
	// the for loop will automatically exit when the channel is closed
	for url := range inch {
		hitUrlAndReport(url, outch)
	}
}

// An Agent goroutine initiated just to process performance on a 'batch' of URLs from the Manager
func agentProcess(pr ProcessRequest, threads int) {
	// to process these urls, spawn off goroutines equivalent to the number of threads
	// create 2 channels, one for sending requests to the goroutines and another for receiving their performance results
	// each one is to be sized to the number of urls, so there will be no blocking
	size := len(pr.Urls)
	outch := make(chan string, size)
	inch := make(chan HitInfo, size)

	for i := 0; i < threads; i++ {
		go agentSubProcess(i, outch, inch) // note inch and outch are reversed for the subprocess because it sees mirror image of in and out
	}

	// dump all URLs on outch, we have that much buffering anyway
	for _, url := range pr.Urls {
		outch <- url
	}

	close(outch) // all values have been sent on it, close to indicate done

	// now keep reading HitInfo from the in channel
	// when we have read responses = size then we know we are done
	respdata := make([]HitInfo, size, size) // we use a fixed size and capacity slice
	i := 0
agentprocessloop:
	for {
		select {
		// read from the inbound channel into the slice
		case respdata[i] = <-inch:
			i++

			if i == size { // TODO: how do we handle a few URLs that are taking too long, should we abandon?
				break agentprocessloop
			}
		}
	}

	// this could be dangerous in theory, the receiver is closing a channel, a no-no in Go
	// this could cause the sender to panic, which we handle via a defer on the agentSubProcess
	// this should not happen in current code because we close only after *all* responses are received, but if we change this to abort slow goroutines, we may hit this
	close(inch)

	// send the response to the manager as a json object which is a slice of HitInfos
	body, err := json.Marshal(respdata)
	if err != nil {
		// marshaling errors are serious and irrecoverable
		panic("Marshaling of the response data failed err = " + err.Error())
	}

	// do the POST to the manager
	resp, err := http.Post(pr.ManagerUrl+"/hitinfo/"+strconv.Itoa(pr.AgentNumber), "text/plain", strings.NewReader(string(body)))
	if err != nil || resp.StatusCode != 200 {
		glog.Errorf("Error when trying to send hitinfo back to manager with url %s resp=%v err=%v", pr.ManagerUrl, resp, err)
	} else if err == nil {
		defer resp.Body.Close()
		io.Copy(ioutil.Discard, resp.Body) // discard the response body
	}
}

// function to handle the main code of an Agent
// does not return from this method since it listens for ever on the Agent http web service
// Spawns off a goroutine to process a request from the manager when it gets a hit on the /process URL
func handleAgent(port int, threads int) {
	// use a pre-specified port or pick a random free port to use
	if port == 0 {
		port = freeport.GetPort()
		fmt.Printf("Will use port %d to listen on since parameter agentport was not specified\n", port)
	}

	// add a default handler for testing if the agent is alive
	http.HandleFunc("/test", func(w http.ResponseWriter, r *http.Request) {
	})

	// add a default handler for being commanded by a manager to start a perf run
	http.HandleFunc("/process", func(w http.ResponseWriter, r *http.Request) {
		// decode the Body as a JSON object
		dec := json.NewDecoder(r.Body)
		var pr ProcessRequest // the body of the response is just a ProcessRequest
		if err := dec.Decode(&pr); err != nil {
			glog.Errorf("Unmarshal of data sent by the manager gave an error = %s", err.Error())
			return
		}

		// spawn off a goroutine to process these urls and respond back the manager (handler will automatically call it as 200 OK)
		go agentProcess(pr, threads)
	})

	listenaddr := fmt.Sprintf(":%d", port) // listen on :port, ip need not be specified

	// should never exit from method below, if it does it is because of an error
	err := http.ListenAndServe(listenaddr, nil)
	// we shouldn't ever return from ListenAndServer unless there is an error
	fmt.Println(err)
	os.Exit(1)
}

// Used by a Manager to confirm that an Agent is alive before running any tests
func testAgent(url string) bool {
	// Use the /test URL to test the liveness of the agent
	resp, err := http.Get("http://" + url + "/test")
	if err != nil {
		glog.Errorf("Error when hitting agent url %s, err = %v", url, err)
		return false
	}

	defer resp.Body.Close()
	io.Copy(ioutil.Discard, resp.Body) // discard the body

	return resp.StatusCode == 200 // true if 200, false otherwise
}

// function to send a random batch of URLs for performance metrics to the Agent
// sends an HTTP request with a ProcessRequest as the data. The agent processes it asynchronously and reports back results using the manager's /hitinfo URL
func sendUrlBatchToAgent(agentNumber int, agentUrl string, managerUrl string, targetUrls []string, count int) {
	// Create a ProcessRequest to send to this agent
	pr := ProcessRequest{ManagerUrl: managerUrl, AgentNumber: agentNumber, Urls: make([]string, count, count)}
	// populate it with urls at random
	for i := 0; i < count; i++ {
		pr.Urls[i] = targetUrls[rand.Int31n(int32(len(targetUrls)))]
	}

	// encode this into a JSON buffer
	buffer, err := json.Marshal(pr)
	if err != nil {
		// marshaling errors are serious and irrecoverable
		panic("Marshaling of the request data failed err = " + err.Error())
	}

	// send it as a Post to the agent
	resp, err := http.Post("http://"+agentUrl+"/process", "text/plain", strings.NewReader(string(buffer)))
	if err != nil || resp.StatusCode != 200 {
		glog.Errorf("Error when trying to send process to agent %s, got statuscode =%d err=%v", agentUrl, resp.StatusCode, err)
	} else if err == nil {
		defer resp.Body.Close()
		io.Copy(ioutil.Discard, resp.Body) // discard the response body
	}
}

// function to handle the main code of a Manager
// A Manager does not run performance metrics itself, instead takes random batches of URLs and sends it to Agents to perform instead.
// Collects results from all Agents and then provides intermediate and final statistics to the user
func handleManager(urls []string, agentList []string, outch chan HitInfo) {
	// find the ip address where this manager is running so that can be passed to an agent to contact it
	ipaddr, err := externalIP()
	if err != nil {
		glog.Errorf("Unexpected error when trying to find the IP address of this server so agents can contact it. Error = %v\n", err)
		os.Exit(2)
	}

	// the manager should start listening on a random new port for agents to send HitInfo responses back to it
	port := fmt.Sprintf(":%d", freeport.GetPort())

	// construct the managerUrl from the ipaddress and port
	managerUrl := fmt.Sprintf("http://%s%s", ipaddr, port)

	fmt.Printf("Agents will contact this manager on URL %s ... will fail if this url is not reachable from agents.\n", managerUrl)

	// add a default handler for response from the agent
	http.HandleFunc("/hitinfo/", func(w http.ResponseWriter, r *http.Request) {
		// the URL should be of format /hitinfo/<agentnumber>
		if len(r.URL.Path) <= len("/hitinfo/") {
			glog.Errorf("Invalid hitinfo URL from agent, cannot process url = %s", r.URL.Path)
			return
		}

		agentNumber, err := strconv.Atoi(string(r.URL.Path[len("/hitinfo/")]))
		if err != nil {
			glog.Errorf("Invalid hitinfo URL from agent, cannot process url = %s", r.URL.Path)
			return
		}

		// decode the Body as a JSON object
		dec := json.NewDecoder(r.Body)
		var his []HitInfo // the body of the response is just an array of HitInfos
		if err := dec.Decode(&his); err != nil {
			glog.Errorf("Unmarshal of data from url %s gave an error = %s", r.URL.Path, err.Error())
			return
		}

		// send each HitInfo on the channel
		for _, hi := range his {
			outch <- hi
		}

		// now that we have got the results, we can get the agent busy again
		// send them another batch in a different goroutine so we an return from this handler
		// TODO: increase them if the response is quicker than we need
		go sendUrlBatchToAgent(agentNumber, agentList[agentNumber], managerUrl, urls, 10)
	})

	// validate if the agent urls are good, if not, exit, we validate by sending them a /test url
	for _, url := range agentList {
		if !testAgent(url) {
			glog.Error("Please check your agents and then restart the test. Exiting\n")
			os.Exit(2)
		}
	}

	// send each agent a batch of urls to kick things off, when we get their responses, we'll send them another batch automatically
	for id, url := range agentList {
		// we start off with a small batch count, of 10 URLs, TODO: increase them if the response is quicker than we need
		sendUrlBatchToAgent(id, url, managerUrl, urls, 10)
	}

	// start a web service to listen on responses from agents
	go http.ListenAndServe(port, nil)
}

// a method to get the external (non-localhost) IP address of the host this manager is running on
// taken from http://stackoverflow.com/questions/23558425/how-do-i-get-the-local-ip-address-in-go
// works only with ipV4 addresses
func externalIP() (string, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return "", err
	}

	// iterate over all interfaces and ignore ones where the interface is down or loopback addresses
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 {
			continue // interface down
		}

		if iface.Flags&net.FlagLoopback != 0 {
			continue // loopback interface
		}

		addrs, err := iface.Addrs()
		if err != nil {
			return "", err
		}

		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}

			if ip == nil || ip.IsLoopback() {
				continue
			}

			ip = ip.To4()
			if ip == nil {
				continue // not an ipv4 address
			}

			return ip.String(), nil
		}
	}

	return "", errors.New("No interfaces found, are you connected on the network?")
}

// function to handle the main code when this is running as a stand-alone, i.e. not in a distibuted setup of Manager and Agent.
// picks a random set of URLs and spawns off required number of goroutines and calculates performance metrics on each one
func handleSolo(urls []string, runfor int, threads int, outch chan HitInfo) {

	// go routine which will make hits forever
	forever := func() {
		for {
			// pick up a random url from the slice
			url := urls[rand.Int31n(int32(len(urls)))]
			hitUrlAndReport(url, outch)
		}
	}

	// start required number of go routines in parallel
	for i := 0; i < threads; i++ {
		go forever()
	}
}

// main function that loads the url files and fires off goroutines based on whether it is running in distributed mode (Agent-Manager)
// or as a stand-alone (solo)
// and keeps tab on the stats in the main thread
func main() {
	// variables to load into from the command line flags
	var urlfile string
	var runfor int = 60
	var threads int = 1
	var isAgent bool = false
	var agentport int = 0
	var agentList string = ""

	// command line flags
	flag.StringVar(&urlfile, "urlfile", "", "path to file with list of urls")
	flag.IntVar(&runfor, "runfor", 60, "how long to run the program for (secs)")
	flag.IntVar(&threads, "threads", 1, "number of parallel threads")
	flag.BoolVar(&isAgent, "agent", false, "run this instance as an agent")
	flag.IntVar(&agentport, "agentport", 0, "use this port for agent to listen on, if not provided will choose any free port")
	flag.StringVar(&agentList, "agenturls", "", "run in manager mode with execution done by agents, where urls of the agents are a comma separated list")

	// parse the command line
	flag.Parse()

	if isAgent {
		// we are an agent, we will listen on a port for process requests and then submit results when done
		handleAgent(agentport, threads)
	} else {
		// a Manager or Solo requires the URL file, agents do not
		if urlfile == "" {
			os.Stderr.WriteString("Missing required parameter urlfile\n")
			os.Exit(1)
		}

		// list of URLs to GET
		urls := readfile(urlfile)

		// seed it so the random number generation (random URLs to hit) is actually somewhat random
		rand.Seed(int64(time.Now().Second()))

		// make a channel for the threads to send hit info back to the main thread
		ch := make(chan HitInfo, threads*2) // buffer size equal to twice number of parallel goroutines

		if agentList != "" {
			// we run as a manager, i.e. we dont run any hits ourselves but get the agents to do them for us
			go handleManager(urls, strings.Split(agentList, ","), ch) // break the agentList into individual urls by comma
		} else {
			// no manager, we run solo
			handleSolo(urls, runfor, threads, ch)
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
				mean, median, sd := stats(msecSlice)
				fmt.Printf("Final result:\n num200OK=%v numFail=%v bytes=%v avgTimeMsec=%v, medianTimeMsec=%v, stddevTimeMsec=%.2f\n", count200, countfail, bytefmt.ByteSize(bytes), mean, median, sd)
				break forloop // end the program
			case <-ticksC: // time for us to print stats until now
				mean := int64(0)
				if count200 > 0 {
					mean = timeMsec / count200
				}
				fmt.Println(count200, countfail, bytes, mean)
			case info := <-ch: // we got hit info from a thread
				glog.V(2).Info(info)
				if info.Status == 200 {
					count200++
					msecSlice = append(msecSlice, info.Timemsec)
					timeMsec += int64(info.Timemsec)
					bytes += uint64(info.Contentlen)
				} else {
					countfail++
				}
			}
		}
	}
}
