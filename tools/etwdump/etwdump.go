package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unsafe"

	"github.com/0xrawsec/golang-etw/etw"
	"github.com/0xrawsec/golang-utils/log"
)

func getAccessString(guid string) (s string) {
	var err error

	g := etw.MustGUIDFromString(guid)
	bSize := uint32(0)
	// retrieves size
	etw.EventAccessQuery(g, nil, &bSize)
	buffer := make([]byte, bSize)
	sd := (*etw.SecurityDescriptor)(unsafe.Pointer(&buffer[0]))
	// we get the security descriptor
	etw.EventAccessQuery(g, sd, &bSize)

	if s, err = etw.ConvertSecurityDescriptorToStringSecurityDescriptorW(
		sd,
		etw.SDDL_REVISION_1,
		etw.DACL_SECURITY_INFORMATION); err != nil {
		panic(err)
	}
	return s
}

func main() {
	var (
		debug               bool
		listKernelProviders bool
		access              bool
		attach              string
		regex               string
		outfile             string
		cregex              *regexp.Regexp
		kernelTraceFlags    uint32

		producers []*etw.RealTimeSession

		sessionName = "EtwdumpTraceSession"
		sessions    = make([]string, 0)
		writer      = os.Stdout
	)

	flag.StringVar(&sessionName, "s", sessionName, "ETW session name")
	flag.StringVar(&attach, "a", attach, "Attach to existing session(s) (comma separated)")
	flag.StringVar(&regex, "e", regex, "Regex to filter in events")
	flag.StringVar(&outfile, "o", outfile, "Output file")
	flag.BoolVar(&access, "access", access, "List accesses to GUIDs")
	flag.BoolVar(&debug, "debug", debug, "Enable debug messages")
	flag.BoolVar(&listKernelProviders, "lk", listKernelProviders, "List kernel providers")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s [OPTIONS] PROVIDERS...\n", filepath.Base(os.Args[0]))
		fmt.Fprintf(os.Stderr, "Options:\n")
		flag.PrintDefaults()
		os.Exit(0)
	}

	flag.Parse()

	if debug {
		log.SetLogLevel(log.LDebug)
	}

	log.Debugf("PID: %d", os.Getpid())

	// list kernel providers
	if listKernelProviders {
		fmt.Println("Kernel Providers")
		for _, pd := range etw.KernelProviders {
			fmt.Printf("\t%s: %s\n", pd.Name, pd.GUID)
		}
		os.Exit(0)
	}

	if access {
		fmt.Println("Listing access rights")
		for _, provider := range flag.Args() {
			fmt.Printf("%s: %s\n", provider, getAccessString(provider))
		}
		os.Exit(0)
	}

	// build up regex
	if regex != "" {
		cregex = regexp.MustCompile(regex)
	}

	// opening output file if needed
	if outfile != "" {
		if fd, err := os.Create(outfile); err != nil {
			log.Errorf("Failed to open output file: %s", err)
		} else {
			writer = fd
		}
	}

	// We create a private producer
	p := etw.NewRealTimeProducer(sessionName)

	// We process the providers provided in the command line
	for _, provider := range flag.Args() {
		// this is a kernel provider
		if etw.IsKernelProvider(provider) {
			log.Debugf("Enabling kernel provider: %s", provider)
			kernelTraceFlags |= etw.GetKernelProviderFlags(provider)
		} else {
			log.Debugf("Enabling provider: %s", provider)
			if err := p.EnableVerboseProvider(provider); err != nil {
				log.Errorf("Failed to enable provider %s: %s", provider, err)
			}
		}
	}

	// We enable producer only if it has at least a provider
	if len(p.Providers) > 0 {
		producers = append(producers, p)
	}

	// We will start kernel producer only if necessary
	if kernelTraceFlags != 0 {
		kp := etw.NewKernelRealTimeProducer(kernelTraceFlags)
		producers = append(producers, kp)
	}

	for _, p := range producers {
		log.Debugf("Starting producer: %s", p.TraceName)
		if err := p.Start(); err != nil {
			panic(err)
		}
		sessions = append(sessions, p.TraceName)
	}

	/** Consumer part **/
	c := etw.NewRealTimeConsumer(context.Background())

	// additional sessions to trace (already started)
	if attach != "" {
		sessions = append(sessions, strings.Split(attach, ",")...)
	}

	for _, s := range sessions {
		log.Debugf("Consumer open trace session: %s", s)
		if err := c.OpenTrace(s); err != nil {
			log.Errorf("Failed to attach to session %s: %s", s, err)
		}
	}

	c.Start()

	// Signal handler to catch interrupt
	h := make(chan os.Signal, 1)
	signal.Notify(h, os.Interrupt)
	go func() {
		<-h
		log.Infof("Received signal Interrupt")

		log.Debug("Stopping producers")
		for _, p := range producers {
			if err := p.Stop(); err != nil {
				log.Errorf("Failed to stop producer: %s", err)
			}
		}
		time.Sleep(500 * time.Millisecond)

		log.Debug("Stopping consumer")
		if err := c.Stop(); err != nil {
			log.Errorf("Error while stopping consumer: %s", err)
		}
	}()

	log.Debug("Consuming events")
	for e := range c.Events {
		if b, err := json.Marshal(&e); err != nil {
			panic(err)
		} else {
			if cregex != nil {
				if cregex.Match(b) {
					fmt.Fprintf(writer, "%s\n", string(b))
				}
			} else {
				fmt.Fprintf(writer, "%s\n", string(b))
			}
		}
	}

	c.Wait()
}