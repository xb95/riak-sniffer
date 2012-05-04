/*
 * riak-sniffer.go
 *
 * A straightforward program for sniffing Riak proto-buffer streams and providing
 * diagnostic information on the realtime queries your database is handling.
 *
 * FIXME: this assumes IPv4.
 *
 * Taken from:
 *    https://github.com/xb95/riak-sniffer
 *
 * See the LICENSE file at the above link for licensing terms.
 *
 * Written by Mark Smith <mark@qq.is> for Bump Technologies (http://bu.mp/).
 *
 */

package main

import (
	"code.google.com/p/goprotobuf/proto"
	"flag"
	"fmt"
	"github.com/akrennmair/gopcap"
	"log"
	"riakclient"
	"sort"
	"time"
)

type riakSourceChannel chan ([]byte)
type riakSource struct {
	src    string
	synced bool
	ch     riakSourceChannel
}
type riakMessage struct {
	method string
	bucket []byte
	key    []byte
}

var start int64 = UnixNow()
var qbuf map[string]int = make(map[string]int)
var querycount int
var chmap map[string]riakSource = make(map[string]riakSource)
var verbose bool = false

func UnixNow() int64 {
	return time.Now().Unix()
}

func main() {
	var port *int = flag.Int("P", 8087, "Riak protocol buffer port")
	var eth *string = flag.String("i", "eth0", "Interface to sniff")
	var snaplen *int = flag.Int("s", 1024, "Bytes of each packet to sniff")
	var period *int = flag.Int("t", 10, "Seconds between outputting status")
	var displaycount *int = flag.Int("d", 25, "Display this many queries in status updates")
	var doverbose *bool = flag.Bool("v", false, "Print every query received (spammy)")
	flag.Parse()

	verbose = *doverbose

	log.SetPrefix("")
	log.SetFlags(0)

	log.Printf("Initializing Riak sniffing on %s:%d...", *eth, *port)
	iface, err := pcap.Openlive(*eth, int32(*snaplen), false, 0)
	if iface == nil || err != "" {
		if err == "" {
			err = "unknown error"
		}
		log.Fatalf("Failed to open device: %s", err)
	}

	err = iface.Setfilter(fmt.Sprintf("tcp dst port %d", *port))
	if err != "" {
		log.Fatalf("Failed to set port filter: %s", err)
	}

	last := UnixNow()
	var pkt *pcap.Packet = nil
	var rv int32 = 0

	for rv = 0; rv >= 0; {
		for pkt, rv = iface.NextEx(); pkt != nil; pkt, rv = iface.NextEx() {
			handlePacket(pkt)

			// simple output printer... this should be super fast since we expect that a
			// system like this will have relatively few unique queries once they're
			// canonicalized.
			if !verbose && querycount%100 == 0 && last < UnixNow()-int64(*period) {
				last = UnixNow()
				handleStatusUpdate(*displaycount)
			}
		}
	}
}

func handleStatusUpdate(displaycount int) {
	elapsed := float64(UnixNow() - start)

	// print status bar
	log.Printf("\n")
	log.SetFlags(log.Ldate | log.Ltime)
	log.Printf("%d total queries, %0.2f per second", querycount, float64(querycount)/elapsed)
	log.SetFlags(0)

	// we cheat so badly here...
	var tmp sort.StringSlice = make([]string, 0, len(qbuf))
	for q, c := range qbuf {
		tmp = append(tmp, fmt.Sprintf("%6d  %0.2f/s  %s", c, float64(c)/elapsed, q))
	}
	sort.Sort(tmp)

	// now print top to bottom, since our sorted list is sorted backwards
	// from what we want
	if len(tmp) < displaycount {
		displaycount = len(tmp)
	}
	for i := 1; i <= displaycount; i++ {
		log.Printf(tmp[len(tmp)-i])
	}
}

// given a string, return a string with safe-to-print bytes
func safe_output(inp []byte) string {
    out := ""
    for _, v := range inp {
        if v >= 32 && v <= 126 {
            out += string(v)
        } else {
            out += fmt.Sprintf("\\x%02x", v)
        }
    }
    return out
}

// Listens on a channel for bytes. This is how we get data in from the various
// clients that are talking to Riak.
func riakSourceListener(rs *riakSource) {
	for {
		data := <-rs.ch

		// Here we need to do the sync logic. This is probably fairly
		// prone to failure, but the idea is to try to look for a packet
		// that is in a format we can parse. If we can do that, then
		// we know exactly where the packet boundaries are and we can
		// reliably parse this stream of bytes. Until a source is in the
		// synced state, we want to ignore its data.
		if !rs.synced {
			datalen := uint32(len(data))

			// Must have 4 bytes (len) + byte (type) + payload. I don't know
			// what the minimum payload length is... assuming 1 for now.
			if datalen < 6 {
				continue
			}

			// Get the length value and then bail if this packet doesn't
			// contain a single request. It'd be nice if we didn't have this
			// requirement, but it's probably OK.
			size := uint32(data[0])<<24 + uint32(data[1])<<16 + uint32(data[2])<<8 + uint32(data[3])
			if datalen != size+4 {
				continue
			}

			// Now see if we can possibly parse out the proto from this
			// packet or if we get gibberish.
			msg, err := getProto(data[4], data[5:])
			if err != nil {
				continue
			}

			// If we actually got a message, then we're synced.
			if msg != nil {
				querycount++
				//text := fmt.Sprintf("%s %s %s:%x", rs.src, (*msg).method, (*msg).bucket, (*msg).key)
				text := fmt.Sprintf("%s %s:%s", (*msg).method, (*msg).bucket, safe_output((*msg).key))
				//text := fmt.Sprintf("%s", rs.src)
				if verbose {
					log.Printf("%s", text)
				} else {
					qbuf[text]++
				}
			}

			//rs.synced = true
			//log.Printf("%s sent %d bytes (type=%d) and is synced", rs.src, len(data), data[4])
			continue
		}

		// We're in sync. Start storing bytes until we get a full packet.
		//log.Printf("%s sent %d bytes (IN SYNC)", rs.src, len(data))
	}
}

// Given a set of bytes and a type, return a protocol buffer object.
func getProto(msgtype byte, data []byte) (*riakMessage, error) {
	var ret *riakMessage = nil

	switch msgtype {
	case 0x09:
		obj := &riakclient.RpbGetReq{}
		err := proto.Unmarshal(data, obj)
		if err != nil {
			return nil, err
		}

		ret = &riakMessage{method: "get", bucket: []byte(obj.Bucket), key: []byte(obj.Key)}
	case 0x0B:
		obj := &riakclient.RpbPutReq{}
		err := proto.Unmarshal(data, obj)
		if err != nil {
			return nil, err
		}

		ret = &riakMessage{method: "put", bucket: []byte(obj.Bucket), key: []byte(obj.Key)}
	}

	return ret, nil
}

// Given a source ("ip:port" string), return a channel that can be used to send
// payload bytes to. If that channel doesn't exist, it sets one up.
func getChannel(src *string) riakSourceChannel {
	rs, ok := chmap[*src]
	if !ok {
		rs = riakSource{src: *src, synced: false, ch: make(riakSourceChannel, 10)}
		go riakSourceListener(&rs)
		chmap[*src] = rs
	}
	return rs.ch
}

// extract the data... we have to figure out where it is, which means extracting data
// from the various headers until we get the location we want.  this is crude, but
// functional and it should be fast.
func handlePacket(pkt *pcap.Packet) {
	// Ethernet frame has 14 bytes of stuff to ignore, so we start our root position here
	var pos byte = 14

	// Grab the src IP address of this packet from the IP header.
	srcIP := pkt.Data[pos+12 : pos+16]

	// The IP frame has the header length in bits 4-7 of byte 0 (relative).
	pos += pkt.Data[pos] & 0x0F * 4

	// Grab the source port from the TCP header.
	srcPort := uint16(pkt.Data[pos])<<8 + uint16(pkt.Data[pos+1])

	// The TCP frame has the data offset in bits 4-7 of byte 12 (relative).
	pos += byte(pkt.Data[pos+12]) >> 4 * 4

	// If this is a 0-length payload, do nothing. (Any way to change our filter
	// to only dump packets with data?)
	if len(pkt.Data[pos:]) <= 0 {
		return
	}

	// Now we have the source and payload information, we can pass this off to
	// somebody who is better equipped to process it.
	src := fmt.Sprintf("%d.%d.%d.%d:%d", srcIP[0], srcIP[1], srcIP[2], srcIP[3], srcPort)
	getChannel(&src) <- pkt.Data[pos:]
}
