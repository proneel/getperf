# getperf
A simple tool to collect statistics on responsiveness of web services.

While I normally use [Apache Bench](https://httpd.apache.org/docs/2.2/programs/ab.html) to do load testing of web services with HTTP GETs, ab unfortunately only supports a single URL to be provided.

There are forks of ab which permit you to give a file or URLs, or use [Siege](https://www.joedog.org/siege-home/) or maybe [JMeter](http://jmeter.apache.org/) or a host of other such tools. Apache Bench and Siege do not allow for hits from distributed 'agents' co-ordinated from a single manager (it is a single-process model essentially), but this tool (getperf) does. Apache JMeter can be run in a distributed mode.

In the interest of learning Go however, I wrote a simple tool which gives me responsiveness statistics on a list of URLs similar to Apache Bench.

###Usage: Non-distributed###
Assuming you have a text file urls.txt with any number of URLs in it, one per line, you could
```
./getperf -urlfile ./urls.txt
```
This will run a single-threaded HTTP client which will pick random URLs frm the file and calculate the response time and bytes read in each hit
Intermediate results will be printed every few seconds and a final result will be delivered after 1 minute such as
```
num200OK=134 numFail=2 bytes=453.1K avgTimeMsec=446, medianTimeMsec=424, stddevTimeMsec=21.18
```
indicating we got 134 success, 2 failures, 453.1K bytes read and the mean/median/standard-deviation of time taken for each response is 446/424/21.18 respectively

One could add more threads (parallel goroutines actually) by
```
./getperf -urlfile ./urls.txt -threads=4
```
or to run for more or less than the default of 1 minute, where the ```runfor``` argument takes the seconds to run for
```
./getperf -urlfile ./urls.txt -runfor=120
```
###Usage: Distributed###
getperf can also be run in a distributed model to simulate a better load test. Multiple 'agents' could be run on one or more servers, which sit around idle until a 'manager' sends them a performance metric 'batch of urls' to process. The whole process is stateless, in that the agents simply process a batch and report results to the manager. In theory, there could be multiple managers that send requests to the same agents, however be aware that if more than one batch is processed in parallel, more threads could execute, affecting the reporting times to some extent.

In distributed mode, the manager does not hit any URLs itself, it simply compiles a batch of random ones from the file and sends it off to agents. Once one batch has been responded to, it sends another batch to process. It aggregates results from all agents and reports it to the user.

Agents could run different number of threads; this could reflect the number of cores on that server or generically any other metric to reflect resource availability on the agent. Note that true performance of the system under test is only calculated if variances on the clients themselves are as minimal as possible (e.g. no throttling as a result of client resource constraints).

To start an agent, simply run:
```
./getperf -agent=true
```
in which case the agent will listen on a random free port (and report it to the user so they could inform the manager on how to
communicate to this agent. Or specify the port by
```
./getperf -agent=true -agentport=3333
```
or in either case, specify parallelism in this agent by multi-threading (default is 1 thread if unspecified)
```
./getperf -agent=true -threads=4
```
To run a manager, simply give references to all such agents (ipaddress:port) and the manager will use all of the provided agents to process the performance hits.
```
./getperf -urlfile ./urls.txt -agenturls=192.168.0.101:2222,192.168.0.107:2222,192.168.0.107:3333
```
