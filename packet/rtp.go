package packet

import (
	"errors"
	"math/rand"
	"net"
	"strconv"
	"time"

	log "github.com/sirupsen/logrus"
)

//RtpTransfer ...
type RtpTransfer struct {
	datasrc      string
	protocol     int // tcp or udp
	psEnc        *encPSPacket
	payload      chan []byte
	cseq         uint16
	ssrc         uint32
	udpconn      *net.UDPConn
	tcpconn      net.Conn
	writestop    chan bool
	quit         chan bool
	timerProcess *time.Ticker
}

func NewRtpService(src string, pro int) *RtpTransfer {

	return &RtpTransfer{
		datasrc:   src,
		protocol:  pro,
		psEnc:     &encPSPacket{},
		payload:   make(chan []byte, 25),
		cseq:      0,
		ssrc:      rand.Uint32(),
		writestop: make(chan bool, 1),
		quit:      make(chan bool, 1),
	}
}

//Service ...
func (rtp *RtpTransfer) Service(srcip, dstip string, srcport, dstport int) error {

	if nil == rtp.timerProcess {
		rtp.timerProcess = time.NewTicker(time.Second * time.Duration(5))
	}
	if rtp.protocol == TCPTransferPassive {
		go rtp.write4tcppassive(srcip+":"+strconv.Itoa(srcport),
			dstip+":"+strconv.Itoa(dstport))

	} else if rtp.protocol == TCPTransferActive {
		// connect to to dst ip port

	} else if rtp.protocol == UDPTransfer {
		conn, err := net.DialUDP("udp",
			&net.UDPAddr{
				IP:   net.ParseIP(srcip),
				Port: srcport,
			},
			&net.UDPAddr{
				IP:   net.ParseIP(dstip),
				Port: dstport,
			})
		if err != nil {
			return err
		}
		rtp.udpconn = conn
		go rtp.write4udp()
	} else {
		return errors.New("unknown transfer way")
	}
	return nil
}

//Exit ...
func (rtp *RtpTransfer) Exit() {

	if nil != rtp.timerProcess {
		rtp.timerProcess.Stop()
	}
	close(rtp.writestop)
	<-rtp.quit
}

func (rtp *RtpTransfer) Send2data(data []byte, key bool, pts uint64) {
	psSys := rtp.psEnc.addPSHeader(pts)
	if key { // just I frame will add this
		psSys = rtp.psEnc.addSystemHeader(psSys, 2048, 512)
	}
	psSys = rtp.psEnc.addMapHeader(psSys)
	lens := len(data)
	var index int
	for lens > 0 {
		pesload := lens
		if pesload > PESLoadLength {
			pesload = PESLoadLength
		}
		pes := rtp.psEnc.addPESHeader(data[index:index+pesload], StreamIDVideo, pesload, pts, pts)

		// every frame add ps header
		if index == 0 {
			pes = append(psSys, pes...)
		}
		// remain data to package again
		// over the max pes len and split more pes load slice
		index += pesload
		lens -= pesload
		if lens > 0 {
			rtp.fragmentation(pes, pts, 0)
		} else {
			// the last slice
			rtp.fragmentation(pes, pts, 1)

		}

	}
}

func (rtp *RtpTransfer) fragmentation(data []byte, pts uint64, last int) {
	datalen := len(data)

	if datalen+RTPHeaderLength <= RtpLoadLength {
		payload := rtp.addRtpHeader(data[:], 1, pts)
		rtp.payload <- payload
	} else {
		marker := 0
		var index int
		sendlen := RtpLoadLength - RTPHeaderLength
		for datalen > 0 {
			if datalen <= sendlen {
				marker = 1
				sendlen = datalen
			}
			payload := rtp.addRtpHeader(data[index:index+sendlen], marker&last, pts)
			rtp.payload <- payload
			datalen -= sendlen
			index += sendlen
		}
	}
}
func (rtp *RtpTransfer) addRtpHeader(data []byte, marker int, curpts uint64) []byte {
	rtp.cseq++
	pack := make([]byte, RTPHeaderLength)
	bits := bitsInit(RTPHeaderLength, pack)
	bitsWrite(bits, 2, 2)
	bitsWrite(bits, 1, 0)
	bitsWrite(bits, 1, 0)
	bitsWrite(bits, 4, 0)
	bitsWrite(bits, 1, uint64(marker))
	bitsWrite(bits, 7, 96)
	bitsWrite(bits, 16, uint64(rtp.cseq))
	bitsWrite(bits, 32, curpts)
	bitsWrite(bits, 32, uint64(rtp.ssrc))
	if rtp.protocol != UDPTransfer {
		var rtpOvertcp []byte
		lens := len(data) + 12
		rtpOvertcp = append(rtpOvertcp, byte(uint16(lens)>>8), byte(uint16(lens)))
		rtpOvertcp = append(rtpOvertcp, bits.pData...)
		return append(rtpOvertcp, data...)
	}
	return append(bits.pData, data...)

}

func (rtp *RtpTransfer) write4udp() {

	log.Infof(" stream data will be write by(%v)", rtp.protocol)
	for {
		select {
		case data, ok := <-rtp.payload:
			if ok {
				if rtp.udpconn != nil {
					lens, err := rtp.udpconn.Write(data)
					if err != nil || lens != len(data) {
						log.Errorf("write data by udp error(%v), len(%v).", err, lens)
						goto WRITESTOP
					}
				}
			} else {
				log.Error("rtp data channel closed")
				goto WRITESTOP
			}
		case <-rtp.timerProcess.C:
			log.Error("channel recv data timeout")
			goto WRITESTOP
		case <-rtp.writestop:
			log.Error("udp rtp send channel stop")
			goto WRITESTOP
		}
	}
WRITESTOP:
	rtp.udpconn.Close()
	rtp.quit <- true
}

func (rtp *RtpTransfer) write4tcppassive(srcaddr, dstaddr string) {

	addr, err := net.ResolveTCPAddr("tcp", srcaddr)
	if err != nil {
		log.Errorf("net.ResolveTCPAddr error(%v).", err)
		return
	}
	listener, err := net.ListenTCP("tcp", addr)
	for {
		conn, err := listener.Accept()
		if err != nil {
			continue
		}
		rtp.tcpconn = conn
		break
	}
	for {
		if rtp.tcpconn == nil {
			goto WRITESTOP
		}
		select {
		case data, ok := <-rtp.payload:
			if ok {
				lens, err := rtp.tcpconn.Write(data)
				if err != nil || lens != len(data) {
					log.Errorf("write data by tcp error(%v), len(%v).", err, lens)
					goto WRITESTOP
				}

			} else {
				log.Errorf("data channel closed")
				goto WRITESTOP
			}
		case <-rtp.timerProcess.C:
			log.Error("channel write data timeout")
			goto WRITESTOP
		case <-rtp.writestop:
			log.Error("tcp rtp send channel stop")
			goto WRITESTOP
		}
	}
WRITESTOP:
	rtp.tcpconn.Close()
	rtp.quit <- true
}
