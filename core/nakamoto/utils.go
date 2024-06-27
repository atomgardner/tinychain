package nakamoto

import (
	"math/big"
	"time"
	"encoding/hex"
	"net"
	"github.com/pion/stun"
	"log"
)

func Timestamp() uint64 {
	now := time.Now()
	milliseconds := now.UnixMilli()
	return uint64(milliseconds)
}

func BigIntToBytes32(i big.Int) (fbuf [32]byte) {
	buf := make([]byte, 32)
	i.FillBytes(buf)
	copy(fbuf[:], buf)
	return fbuf
}

func Bytes32ToBigInt(b [32]byte) big.Int {
	return *new(big.Int).SetBytes(b[:])
}

func Bytes32ToString(b [32]byte) string {
	sl := b[:]
	return hex.EncodeToString(sl)
}

func HexStringToBytes32(s string) [32]byte {
	b, _ := hex.DecodeString(s)
	var fbuf [32]byte
	copy(fbuf[:], b)
	return fbuf
}

func Bytes32ToHexString(b [32]byte) string {
    return hex.EncodeToString(b[:])
}

func PadBytes(src []byte, length int) []byte {
    if len(src) >= length {
        return src
    }
    padding := make([]byte, length-len(src))
    return append(padding, src...)
}

func DiscoverIP() (string, int, error) {
    // Create a UDP listener
    localAddr := "[::]:0" // Change port if needed
    conn, err := net.ListenPacket("udp", localAddr)
    if err != nil {
        log.Fatalf("Failed to listen on UDP port: %v", err)
    }
    defer conn.Close()
    // localAddr2 := conn.LocalAddr().(*net.UDPAddr)
    // fmt.Printf("Random UDP port: %d\n", localAddr2.Port)
    // fmt.Printf("Listening on %s\n", localAddr)

    // Parse a STUN URI
	u, err := stun.ParseURI("stun:stun.l.google.com:19302")
	if err != nil {
		panic(err)
	}

    // Creating a "connection" to STUN server.
    c, err := stun.DialURI(u, &stun.DialConfig{})
    if err != nil {
        panic(err)
    }
    // Building binding request with random transaction id.
    message := stun.MustBuild(stun.TransactionID, stun.BindingRequest)

    cbChan := make(chan stun.Event, 1)

    // Sending request to STUN server, waiting for response message.
    if err := c.Do(message, func(res stun.Event) {
        cbChan <- res
    }); err != nil {
        panic(err)
    }

    // Waiting for response message.
    res := <-cbChan
    if res.Error != nil {
        panic(res.Error)
    }
    // Decoding XOR-MAPPED-ADDRESS attribute from message.
    var xorAddr stun.XORMappedAddress
    if err := xorAddr.GetFrom(res.Message); err != nil {
        panic(err)
    }

    // Print the external IP and port
    peerLogger.Printf("External IP: %s\n", xorAddr.IP)
    peerLogger.Printf("External Port: %d\n", xorAddr.Port)

    return xorAddr.IP.String(), xorAddr.Port, nil
}