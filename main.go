package main

import (
	"net"
	"log"
	"crypto/rand"
	"fmt"
	"context"
	"time"
	"sync"
	"os"
	"os/signal"
	"net/http"

	"golang.org/x/time/rate"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/insomniacslk/dhcp/dhcpv4/nclient4"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

func generateRandMAC() (net.HardwareAddr, error) {
	buf := make([]byte, 6)
	_, err := rand.Read(buf)
	if err != nil {
		return nil, err
	}

	buf[0] = (buf[0] | 2) & 0xfe // Set local bit, ensure unicast address
	macString := fmt.Sprintf("%02x:%02x:%02x:%02x:%02x:%02x", buf[0], buf[1], buf[2], buf[3], buf[4], buf[5])
	mac, err := net.ParseMAC(macString)
	
	if err != nil {
		return nil, err
	}

	return mac, nil
}

func setHWAddr(c *nclient4.Client, mac net.HardwareAddr) (err error) {
	f := nclient4.WithHWAddr(mac)
	err = f(c)
	return
}

func newReleaseMessage(hwaddr net.HardwareAddr, clientIP net.IP, serverIP net.IP) (*dhcpv4.DHCPv4, error) {
	return dhcpv4.New(
		dhcpv4.WithHwAddr(hwaddr),
		dhcpv4.WithMessageType(dhcpv4.MessageTypeRelease),
		dhcpv4.WithClientIP(clientIP),
		dhcpv4.WithServerIP(serverIP),
	)
}

func worker(c context.Context, limiter *rate.Limiter, wg *sync.WaitGroup) {
	defer wg.Done()

	conn, err := nclient4.NewRawUDPConn("eth0", 68) // broadcast
	if err != nil {
		log.Fatalf("unable to open a broadcasting socket: %w", err)
		return
	}

	i, err := net.InterfaceByName("eth0")
	if err != nil {
		log.Fatalf("unable to get interface information: %w", err)
		return
	}

	client, _ := nclient4.NewWithConn(conn, i.HardwareAddr)

	for {
		select {
		case <-c.Done():
			return
		default:
			limiter.Wait(c)

			randMAC, _ := generateRandMAC()
			err := setHWAddr(client, randMAC)

			conversation := make([]*dhcpv4.DHCPv4, 0)

			// Discover
			// RFC 2131, Section 4.4.1, Table 5 details what a DISCOVER packet should
			// contain.
			discover, err := dhcpv4.NewDiscovery(randMAC)
			if err != nil {
				err = fmt.Errorf("unable to create a discovery request: %w", err)
				return
			}

			conversation = append(conversation, discover)

			// Both server and client only get 2 seconds.
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()

			log.Printf("Discover sent for MAC: %s\n", randMAC.String())
			start := time.Now()
			offer, err := client.SendAndRead(ctx, nclient4.DefaultServers, discover, nclient4.IsMessageType(dhcpv4.MessageTypeOffer))
			discovers.Inc()
		
			// TODO: detect timeout
			if err != nil {
				log.Fatalf("got an error while the discovery request: %w", err)
				return
			}
			offerLatency := float64(time.Since(start).Milliseconds())
			discOfferLatency.Observe(offerLatency)
			conversation = append(conversation, offer)

			// Request and Ack
			request, err := dhcpv4.NewRequestFromOffer(offer)
			if err != nil {
				log.Fatalf("error while creating request: %w", err)
				return
			}
			conversation = append(conversation, request)

			log.Printf("Request for MAC: %s\n", randMAC.String())
			start = time.Now()
			ack, err := client.SendAndRead(ctx, nclient4.DefaultServers, request, nclient4.IsMessageType(dhcpv4.MessageTypeAck))
			requests.Inc()

			if err != nil {
				log.Fatalf("error while sending request: %w", err)
				return
			}
			ackLatency := float64(time.Since(start).Milliseconds())
			requestAckLatency.Observe(ackLatency)
			conversation = append(conversation, ack)

			// send release message
			release, err := newReleaseMessage(randMAC, offer.YourIPAddr, offer.ServerIdentifier())
			if _, err := conn.WriteTo(release.ToBytes(), nclient4.DefaultServers); err != nil {
				log.Fatalf("error writing packet to connection: %w", err)
				return
			}
		}
	}
}

var (
	discovers = promauto.NewCounter(prometheus.CounterOpts{
		Name: "discover_packets_sent",
		Help: "Number of discover packets sent",
	})
	requests = promauto.NewCounter(prometheus.CounterOpts{
		Name: "request_packets_sent",
		Help: "Number of request packets sent",
	})
	discOfferLatency = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:      "discover_offer_latency",
		Help:      "DISCOVERY-OFFER latency.",
	})
	requestAckLatency = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:      "request_ack_latency",
		Help:      "REQUEST-ACK latency.",
	})
)

func main () {
	limiter := rate.NewLimiter(5, 1)

	ctx := context.Background()
	ctx, cancel := context.WithCancel(ctx)

	// trap Ctrl+C and call cancel on the context
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)
	defer func() {
		signal.Stop(c)
		cancel()
	}()

	go func() {
		select {
		case <-c:
			cancel()
		case <-ctx.Done():
		}
	}()

	go func() {
		http.Handle("/metrics", promhttp.Handler())
		http.ListenAndServe(":2112", nil)
	}()

	var wg sync.WaitGroup
	wg.Add(5)
	for i:=0; i < 5; i++ {
		go worker(ctx, limiter, &wg)
	}
	wg.Wait()
}