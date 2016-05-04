package nsqlookupd

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/absolute8511/nsq/internal/protocol"
	"github.com/absolute8511/nsq/internal/version"
)

type LookupProtocolV1 struct {
	ctx *Context
}

func (p *LookupProtocolV1) IOLoop(conn net.Conn) error {
	var err error
	var line string

	client := NewClientV1(conn)
	reader := bufio.NewReader(client)
	for {
		line, err = reader.ReadString('\n')
		if err != nil {
			break
		}

		line = strings.TrimSpace(line)
		params := strings.Split(line, " ")

		var response []byte
		response, err = p.Exec(client, reader, params)
		if err != nil {
			ctx := ""
			if parentErr := err.(protocol.ChildErr).Parent(); parentErr != nil {
				ctx = " - " + parentErr.Error()
			}
			nsqlookupLog.LogErrorf(" [%s] - %s%s", client, err, ctx)

			_, sendErr := protocol.SendResponse(client, []byte(err.Error()))
			if sendErr != nil {
				nsqlookupLog.LogErrorf(" [%s] - %s%s", client, sendErr, ctx)
				break
			}

			// errors of type FatalClientErr should forceably close the connection
			if _, ok := err.(*protocol.FatalClientErr); ok {
				break
			}
			continue
		}

		if response != nil {
			_, err = protocol.SendResponse(client, response)
			if err != nil {
				break
			}
		}
	}

	nsqlookupLog.Logf("CLIENT(%s): closing", client)
	if client.peerInfo != nil {
		p.ctx.nsqlookupd.DB.RemoveAllByPeerId(client.peerInfo.Id)
	}
	return err
}

func (p *LookupProtocolV1) Exec(client *ClientV1, reader *bufio.Reader, params []string) ([]byte, error) {
	switch params[0] {
	case "PING":
		return p.PING(client, params)
	case "IDENTIFY":
		return p.IDENTIFY(client, reader, params[1:])
	case "REGISTER":
		return p.REGISTER(client, reader, params[1:])
	case "UNREGISTER":
		return p.UNREGISTER(client, reader, params[1:])
	}
	return nil, protocol.NewFatalClientErr(nil, "E_INVALID", fmt.Sprintf("invalid command %s", params[0]))
}

func getTopicChan(command string, params []string) (string, string, string, error) {
	if len(params) <= 1 {
		return "", "", "", protocol.NewFatalClientErr(nil, "E_INVALID", fmt.Sprintf("%s insufficient number of params", command))
	}

	topicName := params[0]
	partitionID := params[1]
	var channelName string
	if len(params) >= 3 {
		channelName = params[2]
	}

	if !protocol.IsValidTopicName(topicName) {
		return "", "", "", protocol.NewFatalClientErr(nil, "E_BAD_TOPIC", fmt.Sprintf("%s topic name '%s' is not valid", command, topicName))
	}

	if channelName != "" && !protocol.IsValidChannelName(channelName) {
		return "", "", "", protocol.NewFatalClientErr(nil, "E_BAD_CHANNEL", fmt.Sprintf("%s channel name '%s' is not valid", command, channelName))
	}

	if _, err := GetValidPartitionID(partitionID); err != nil {
		return "", "", "", protocol.NewFatalClientErr(nil, "E_BAD_PARTITIONID", fmt.Sprintf("%s partition id '%s' is not valid", command, partitionID))
	}
	return topicName, channelName, partitionID, nil
}

func (p *LookupProtocolV1) REGISTER(client *ClientV1, reader *bufio.Reader, params []string) ([]byte, error) {
	if client.peerInfo == nil {
		return nil, protocol.NewFatalClientErr(nil, "E_INVALID", "client must IDENTIFY")
	}

	topic, channel, pid, err := getTopicChan("REGISTER", params)
	if err != nil {
		return nil, err
	}

	if channel != "" {
		key := ChannelReg{
			PartitionID: pid,
			PeerId:      client.peerInfo.Id,
			Channel:     channel,
		}
		if p.ctx.nsqlookupd.DB.AddChannelReg(topic, key) {
			nsqlookupLog.Logf("DB: client(%s) REGISTER new channel: topic:%s channel:%s pid:%s",
				client, topic, channel, pid)
		}
	}
	if p.ctx.nsqlookupd.DB.AddTopicProducer(topic, pid, &Producer{peerInfo: client.peerInfo}) {
		nsqlookupLog.Logf("DB: client(%s) REGISTER new topic:%s pid:%s",
			client, topic, pid)
	}

	return []byte("OK"), nil
}

func (p *LookupProtocolV1) UNREGISTER(client *ClientV1, reader *bufio.Reader, params []string) ([]byte, error) {
	if client.peerInfo == nil {
		return nil, protocol.NewFatalClientErr(nil, "E_INVALID", "client must IDENTIFY")
	}

	topic, channel, pid, err := getTopicChan("UNREGISTER", params)
	if err != nil {
		return nil, err
	}

	if channel != "" {
		key := ChannelReg{
			PartitionID: pid,
			PeerId:      client.peerInfo.Id,
			Channel:     channel,
		}

		removed := p.ctx.nsqlookupd.DB.RemoveChannelReg(topic, key)
		if removed {
			nsqlookupLog.Logf("DB: client(%s) UNREGISTER channel %v on topic:%s-%v",
				client, channel, topic, pid)
		}
	} else {
		// no channel was specified so this is a topic unregistration
		// remove all of the channel registrations...
		// normally this shouldn't happen which is why we print a warning message
		// if anything is actually removed
		key := ChannelReg{
			PartitionID: pid,
			PeerId:      client.peerInfo.Id,
			Channel:     "*",
		}

		removed := p.ctx.nsqlookupd.DB.RemoveChannelReg(topic, key)
		if removed {
			nsqlookupLog.LogErrorf(" client(%s) unexpected UNREGISTER all channels under topic:%s pid:%s",
				client, topic, pid)
		}

		if removed := p.ctx.nsqlookupd.DB.RemoveTopicProducer(topic, pid, client.peerInfo.Id); removed {
			nsqlookupLog.Logf("DB: client(%s) UNREGISTER topic :%s pid:%s",
				client, topic, pid)
		}
	}

	return []byte("OK"), nil
}

func (p *LookupProtocolV1) IDENTIFY(client *ClientV1, reader *bufio.Reader, params []string) ([]byte, error) {
	var err error

	if client.peerInfo != nil {
		return nil, protocol.NewFatalClientErr(err, "E_INVALID", "cannot IDENTIFY again")
	}

	var bodyLen int32
	err = binary.Read(reader, binary.BigEndian, &bodyLen)
	if err != nil {
		return nil, protocol.NewFatalClientErr(err, "E_BAD_BODY", "IDENTIFY failed to read body size")
	}

	body := make([]byte, bodyLen)
	_, err = io.ReadFull(reader, body)
	if err != nil {
		return nil, protocol.NewFatalClientErr(err, "E_BAD_BODY", "IDENTIFY failed to read body")
	}

	// body is a json structure with producer information
	// Id should be identified by the client.
	peerInfo := PeerInfo{Id: ""}
	err = json.Unmarshal(body, &peerInfo)
	if err != nil {
		return nil, protocol.NewFatalClientErr(err, "E_BAD_BODY", "IDENTIFY failed to decode JSON body")
	}

	peerInfo.RemoteAddress = client.RemoteAddr().String()

	// require all fields
	if peerInfo.Id == "" || peerInfo.BroadcastAddress == "" || peerInfo.TCPPort == 0 || peerInfo.HTTPPort == 0 || peerInfo.Version == "" {
		nsqlookupLog.Logf("identify info missing: %v", peerInfo)
		return nil, protocol.NewFatalClientErr(nil, "E_BAD_BODY", "IDENTIFY missing fields")
	}

	atomic.StoreInt64(&peerInfo.lastUpdate, time.Now().UnixNano())

	nsqlookupLog.Logf("CLIENT(%s): IDENTIFY Address:%s TCP:%d HTTP:%d Version:%s",
		client, peerInfo.BroadcastAddress, peerInfo.TCPPort, peerInfo.HTTPPort, peerInfo.Version)

	client.peerInfo = &peerInfo
	if p.ctx.nsqlookupd.DB.addPeerClient(peerInfo.Id, client.peerInfo) {
		nsqlookupLog.Logf("DB: client(%s) REGISTER new peer", client)
	}

	// build a response
	data := make(map[string]interface{})
	data["tcp_port"] = p.ctx.nsqlookupd.RealTCPAddr().Port
	data["http_port"] = p.ctx.nsqlookupd.RealHTTPAddr().Port
	data["version"] = version.Binary
	hostname, err := os.Hostname()
	if err != nil {
		nsqlookupLog.LogErrorf("ERROR: unable to get hostname %s", err)
	}
	data["broadcast_address"] = p.ctx.nsqlookupd.opts.BroadcastAddress
	data["hostname"] = hostname

	response, err := json.Marshal(data)
	if err != nil {
		nsqlookupLog.LogErrorf(" marshaling %v", data)
		return []byte("OK"), nil
	}
	return response, nil
}

func (p *LookupProtocolV1) PING(client *ClientV1, params []string) ([]byte, error) {
	if client.peerInfo != nil {
		// we could get a PING before other commands on the same client connection
		cur := time.Unix(0, atomic.LoadInt64(&client.peerInfo.lastUpdate))
		now := time.Now()
		nsqlookupLog.Logf("CLIENT(%s): pinged (last ping %s)", client.peerInfo.Id,
			now.Sub(cur))
		atomic.StoreInt64(&client.peerInfo.lastUpdate, now.UnixNano())
	}
	return []byte("OK"), nil
}
