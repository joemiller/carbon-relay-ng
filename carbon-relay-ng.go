// carbon-relay-ng
// route traffic to anything that speaks the Graphite Carbon protocol (text or pickle)
// such as Graphite's carbon-cache.py, influxdb, ...
package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"github.com/BurntSushi/toml"
	statsD "github.com/Dieterbe/statsd-go"
	"github.com/graphite-ng/carbon-relay-ng/admin"
	"github.com/rcrowley/goagain"
	"io"
	"log"
	"net"
	"os"
	"runtime/pprof"
	"strings"
)

type StatsdConfig struct {
	Enabled  bool
	Instance string
	Host     string
	Port     int
}

type Blacklist struct {
	Patterns []string
}

type Config struct {
	Listen_addr string
	Admin_addr  string
	Http_addr   string
	Spool_dir   string
	First_only  bool
	Routes      []*Route
	Statsd      StatsdConfig
	Init        []string
}

var (
	config_file string
	config      Config
	to_dispatch = make(chan []byte)
	table       *Table
	statsd      statsD.Client
	cpuprofile  = flag.String("cpuprofile", "", "write cpu profile to file")
)

func init() {
	log.SetFlags(log.Ltime | log.Lmicroseconds | log.Lshortfile)
}

func accept(l *net.TCPListener, config Config) {
	for {
		c, err := l.AcceptTCP()
		if nil != err {
			log.Println(err)
			break
		}
		go handle(c, config)
	}
}

func handle(c *net.TCPConn, config Config) {
	defer c.Close()
	// TODO c.SetTimeout(60e9)
	r := bufio.NewReaderSize(c, 4096)
LineReader:
	for {
		buf, isPrefix, err := r.ReadLine()
		if nil != err {
			if io.EOF != err {
				log.Println(err)
			}
			break
		}
		if isPrefix { // TODO Recover from partial reads.
			log.Println("isPrefix: true")
			break
		}
		for _, blacklist := range config.Blacklist {
			if strings.Contains(string(buf), blacklist.Patt) {
				statsdClient.Increment("target_type=count.unit=Metric.direction=blacklist")
				continue LineReader
			}
		}

		buf = append(buf, '\n')
		buf_copy := make([]byte, len(buf), len(buf))
		copy(buf_copy, buf)
		statsdClient.Increment("target_type=count.unit=Metric.direction=in")
		to_dispatch <- buf_copy
	}
}

func Router() {
	for buf := range to_dispatch {
		routed := table.Dispatch(buf, config.First_only)
		if !routed {
			log.Printf("unrouteable: %s\n", buf)
		}
	}
}

func usage() {
	fmt.Fprintln(
		os.Stderr,
		"Usage: carbon-relay-ng <path-to-config>",
	)
	flag.PrintDefaults()
}

func main() {

	flag.Usage = usage
	flag.Parse()

	config_file = "/etc/carbon-relay-ng.ini"
	if 1 == flag.NArg() {
		config_file = flag.Arg(0)
	}

	if _, err := toml.DecodeFile(config_file, &config); err != nil {
		fmt.Printf("Cannot use config file '%s':\n", config_file)
		fmt.Println(err)
		return
	}
	if *cpuprofile != "" {
		f, err := os.Create(*cpuprofile)
		if err != nil {
			log.Fatal(err)
		}
		pprof.StartCPUProfile(f)
		defer pprof.StopCPUProfile()
	}

	log.Println("creating routing table...")
	table = NewTable(config.Spool_dir, &statsdClient)
	log.Println("initializing routing table...")
	var err error
	for i, cmd := range config.Init {
		err = applyCommand(table, cmd)
		if err != nil {
			log.Println("could not apply init cmd #", i+1)
			log.Println(err)
			os.Exit(1)
		}
	}
	err = routes.Run()
	if err != nil {
		log.Println(err)
		os.Exit(1)
	}

	statsdPrefix := fmt.Sprintf("service=carbon-relay-ng.instance=%s.", config.Statsd.Instance)
	statsdClient = *statsd.NewClient(config.Statsd.Enabled, config.Statsd.Host, config.Statsd.Port, statsdPrefix)

	// Follow the goagain protocol, <https://github.com/rcrowley/goagain>.
	l, ppid, err := goagain.GetEnvs()
	if nil != err {
		laddr, err := net.ResolveTCPAddr("tcp", config.Listen_addr)
		if nil != err {
			log.Println(err)
			os.Exit(1)
		}
		l, err = net.ListenTCP("tcp", laddr)
		if nil != err {
			log.Println(err)
			os.Exit(1)
		}
		log.Printf("listening on %v", laddr)
		go accept(l.(*net.TCPListener), config)
	} else {
		log.Printf("resuming listening on %v", l.Addr())
		go accept(l.(*net.TCPListener), config)
		if err := goagain.KillParent(ppid); nil != err {
			log.Println(err)
			os.Exit(1)
		}
	}

	if config.Admin_addr != "" {
		go func() {
			err := admin.adminListener(config.Admin_addr)
			if err != nil {
				fmt.Println("Error listening:", err.Error())
				os.Exit(1)
			}
		}()
	}

	if config.Http_addr != "" {
		go admin.HttpListener(config.Http_addr, routes, &statsdClient)
	}

	go Router()

	if err := goagain.AwaitSignals(l); nil != err {
		log.Println(err)
		os.Exit(1)
	}
}
