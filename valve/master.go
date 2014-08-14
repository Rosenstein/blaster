// vim: set ts=4 sw=4 tw=99 noet:
package valve

import (
	"bytes"
	"fmt"
	"net"
	"time"
)

const kMaxFilterLength = 190
const kDefaultMasterTimeout = time.Minute * 5

var ErrBadResponseHeader = fmt.Errorf("bad response header")
var kMasterResponseHeader = []byte{0xff, 0xff, 0xff, 0xff, 0x66, 0x0a}
var kNullIP = net.IP([]byte{0, 0, 0, 0})

// A list of IP addresses and ports.
type ServerList []*net.TCPAddr

// The callback the master query tool uses to notify of a batch of servers that
// has just been received.
type MasterQueryCallback func(servers ServerList) error

// Class for querying the master server.
type MasterServerQuerier struct {
	hostAndPort string
	filters     []string
}

// Create a new master server querier on the given host and port.
func NewMasterServerQuerier(hostAndPort string) *MasterServerQuerier {
	return &MasterServerQuerier{
		hostAndPort: hostAndPort,
	}
}

// Adds by AppIds to the filter list.
func (this *MasterServerQuerier) FilterAppIds(appIds []int32) {
	for _, appId := range appIds {
		this.filters = append(this.filters, fmt.Sprintf("\\appid\\%d", appId))
	}
}

func computeNextFilterList(filters []string) ([]string, []string) {
	next := []string{}
	length := 0
	for _, filter := range filters {
		if len(filter) + length >= kMaxFilterLength {
			break
		}
		length += len(filter)
		next = append(next, filter)
	}
	return next, filters[len(next):]
}

// Query the master. Since the master server has timeout problems with lots of
// subsequent requests, we sleep for two seconds in between each batch request.
// This means the querying process is quite slow.
func (this *MasterServerQuerier) Query(callback MasterQueryCallback) error {
	filters, remaining := computeNextFilterList(this.filters)
	for {
		if err := this.tryQuery(callback, filters); err != nil {
			return err
		}

		if len(remaining) == 0 {
			break
		}
		filters, remaining = computeNextFilterList(remaining)
	}
	return nil
}

// Build a packet to query the master server, given an initial starting server
// ("0.0.0.0:0" for the initial batch) and an optional list of filter strings.
func BuildMasterQuery(hostAndPort string, filters []string) []byte {
	packet := PacketBuilder{}
	packet.WriteByte(0x31) // Magic number
	packet.WriteByte(0xFF) // All regions.
	packet.WriteCString(hostAndPort)

	if len(filters) == 0 {
		packet.WriteByte(0)
		packet.WriteByte(0)
	} else {
		header := fmt.Sprintf("\\or\\%d", len(filters))
		packet.WriteBytes([]byte(header))
		for _, filter := range filters {
			packet.WriteBytes([]byte(filter))
		}
		packet.WriteByte(0)
	}
	return packet.Bytes()
}

func (this *MasterServerQuerier) tryQuery(callback MasterQueryCallback, filters []string) error {
	cn, err := NewUdpSocket(this.hostAndPort, kDefaultMasterTimeout)
	if err != nil {
		return err
	}
	defer cn.Close()

	query := BuildMasterQuery("0.0.0.0:0", filters)
	if err = cn.Send(query); err != nil {
		return err
	}

	packet, err := cn.Recv()
	if err != nil {
		return err
	}

	// Sanity check the header.
	if len(packet) < 6 || bytes.Compare(packet[0:6], kMasterResponseHeader) != 0 {
		return ErrBadResponseHeader
	}

	// Chop off the response header.
	packet = packet[6:]

	done := false
	ip := kNullIP
	port := uint16(0)
	for {
		reader := NewPacketReader(packet)
		serverCount := len(packet) / 6

		if serverCount == 0 {
			return fmt.Errorf("expected more than one server in response")
		}

		servers := ServerList{}
		for i := 0; i < serverCount; i++ {
			ip, err = reader.ReadIPv4()
			if err != nil {
				return err
			}
			port, err = reader.ReadPort()
			if err != nil {
				return err
			}

			servers = append(servers, &net.TCPAddr{
				IP: ip,
				Port: int(port),
			})

			// The list is terminated with 0s.
			if ip.Equal(kNullIP) && port == 0 {
				done = true
				break
			}
		}

		if err := callback(servers); err != nil {
			return err
		}

		if done {
			break
		}

		// Attempt to get the next batch 4 more times.
		for i := 1; ; i++ {
			time.Sleep(time.Second * 2)
			address := fmt.Sprintf("%s:%d", ip.String(), port)
			query := BuildMasterQuery(address, filters)
			if err = cn.Send(query); err != nil {
				return err
			}

			if packet, err = cn.Recv(); err == nil {
				// Ok, keep going.
				break
			}

			// Maximum number of retries before we give up.
			if i == 4 {
				return err
			}
		}
	}

	return nil
}