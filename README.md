# getperf
A simple tool to collect statistics on responsiveness of web services.

While I normally use [Apache Bench](https://httpd.apache.org/docs/2.2/programs/ab.html) to do load testing of web services with HTTP GETs, ab unfortunately only supports a single URL to be provided.

There are forks of ab which permit you to give a file or URLs, or use [Siege](https://www.joedog.org/siege-home/) or maybe [JMeter](http://jmeter.apache.org/) or a host of other such tools.

In the interest of learning Go however, I wrote a simple tool which gives me responsiveness statistics on a list of URLs similar to Apache Bench.

###Usage:###
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
