package main

import (
	"errors"
	"log"
	"net"
	"os"
	"os/signal"
	"sync"
	"time"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/perf"
	"github.com/cilium/ebpf/rlimit"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netlink/nl"
)

func main() {
	// Remove resource limits for kernels <5.11.
	if err := rlimit.RemoveMemlock(); err != nil {
		log.Fatal("Removing memlock:", err)
	}

	// Load the compiled eBPF ELF and load it into the kernel.
	var objs bpfObjects
	if err := loadBpfObjects(&objs, nil); err != nil {
		log.Fatal("Loading eBPF objects:", err)
	}
	defer objs.Close()

	// fc00:a:12:0:8000::/80
	target := &net.IPNet{IP: net.ParseIP("fc00:a:12:0:8000::"), Mask: net.CIDRMask(80, 128)}
	bpfEncap := &netlink.BpfEncap{}
	bpfEncap.SetProg(nl.LWT_BPF_XMIT, objs.DoTestData.FD(), "lwt_xmit/test_data")
	route := netlink.Route{
		Dst:      target,
		Encap:    bpfEncap,
		Gw:       net.ParseIP("fc00:a::"),
		Priority: 1,
	}

	if err := netlink.RouteAdd(&route); err != nil {
		log.Fatalf("Failed to add route: %v", err)
	}

	wg := &sync.WaitGroup{}
	stop := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		handleUsLogF(stop, objs.LogEntries)
	}()

	interrupt := make(chan os.Signal, 5)
	signal.Notify(interrupt, os.Interrupt)

	// Wait for the program to be interrupted.
	<-interrupt
	close(stop)

	wg.Wait()
}

func handleUsLogF(stop chan struct{}, m *ebpf.Map) {
	r, err := perf.NewReader(m, os.Getpagesize())
	if err != nil {
		log.Fatalf("Failed to create perf event reader: %v", err)
	}
	defer r.Close()
	evCh := make(chan []byte)

	stopped := false
	done := make(chan struct{})
	go func() {
		defer close(done)
		for !stopped {
			r.SetDeadline(time.Now().Add(500 * time.Millisecond))
			ev, err := r.Read()
			if errors.Is(err, os.ErrDeadlineExceeded) {
				continue
			}
			if err != nil {
				log.Fatalf("Failed to read perf event: %v", err)
			}
			evCh <- ev.RawSample
		}
	}()

	for {
		select {
		case ev := <-evCh:
			log.Println(string(ev))
		case <-stop:
			log.Println("Stopping event reader")
			stopped = true
			<-done
			return
		}
	}
}
