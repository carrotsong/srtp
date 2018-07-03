package srtp

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/binary"

	"github.com/pions/webrtc/pkg/rtp"
	"github.com/pkg/errors"
)

const (
	labelEncryption        = 0x00
	labelSalt              = 0x02
	labelAuthenticationTag = 0x01

	keyLen  = 16
	saltLen = 14

	maxROCDisorder    = 100
	maxSequenceNumber = 65535
)

// Context represents a SRTP cryptographic context
// which is a tuple of <SSRC, destination network address, destination transport port number>
type Context struct {
	ssrc uint32

	rolloverCounter      uint32
	rolloverHasProcessed bool
	lastSequenceNumber   uint16

	masterKey  []byte
	masterSalt []byte

	sessionKey     []byte
	sessionSalt    []byte
	sessionAuthTag []byte

	block cipher.Block
}

/*
  Note to reader
  lines prefixed with '-' are added by Sean-Der
  lines without that prefix are from RFC
*/

// CreateContext creates a new SRTP Context
func CreateContext(masterKey, masterSalt []byte, profile string, ssrc uint32) (c *Context, err error) {
	if masterKeyLen := len(masterKey); masterKeyLen != keyLen {
		return c, errors.Errorf("SRTP Master Key must be len %d, got %d", masterKey, keyLen)
	} else if masterSaltLen := len(masterSalt); masterSaltLen != saltLen {
		return c, errors.Errorf("SRTP Salt must be len %d, got %d", saltLen, masterSaltLen)
	}

	c = &Context{
		masterKey:  masterKey,
		masterSalt: masterSalt,
		ssrc:       ssrc,
	}

	if c.sessionKey, err = c.generateSessionKey(); err != nil {
		return nil, err
	}

	if c.sessionSalt, err = c.generateSessionSalt(); err != nil {
		return nil, err
	}

	if c.sessionAuthTag, err = c.generateSessionAuthTag(); err != nil {
		return nil, err
	}

	c.block, err = aes.NewCipher(c.sessionKey)
	if err != nil {
		return nil, err
	}

	return c, nil
}

func (c *Context) generateSessionKey() ([]byte, error) {
	// https://tools.ietf.org/html/rfc3711#appendix-B.3
	// The input block for AES-CM is generated by exclusive-oring the master salt with the
	// concatenation of the encryption key label 0x00 with (index DIV kdr),
	// - index is 'rollover count' and DIV is 'divided by'
	sessionKey := make([]byte, len(c.masterSalt))
	copy(sessionKey, c.masterSalt)

	labelAndIndexOverKdr := []byte{labelEncryption, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}
	for i, j := len(labelAndIndexOverKdr)-1, len(sessionKey)-1; i >= 0; i, j = i-1, j-1 {
		sessionKey[j] = sessionKey[j] ^ labelAndIndexOverKdr[i]
	}

	// then padding on the right with two null octets (which implements the multiply-by-2^16 operation, see Section 4.3.3).
	sessionKey = append(sessionKey, []byte{0x00, 0x00}...)

	//The resulting value is then AES-CM- encrypted using the master key to get the cipher key.
	block, err := aes.NewCipher(c.masterKey)
	if err != nil {
		return nil, err
	}

	block.Encrypt(sessionKey, sessionKey)
	return sessionKey, nil
}

func (c *Context) generateSessionSalt() ([]byte, error) {
	// https://tools.ietf.org/html/rfc3711#appendix-B.3
	// The input block for AES-CM is generated by exclusive-oring the master salt with
	// the concatenation of the encryption salt label
	sessionSalt := make([]byte, len(c.masterSalt))
	copy(sessionSalt, c.masterSalt)

	labelAndIndexOverKdr := []byte{labelSalt, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}
	for i, j := len(labelAndIndexOverKdr)-1, len(sessionSalt)-1; i >= 0; i, j = i-1, j-1 {
		sessionSalt[j] = byte(sessionSalt[j]) ^ byte(labelAndIndexOverKdr[i])
	}

	// That value is padded and encrypted as above.
	sessionSalt = append(sessionSalt, []byte{0x00, 0x00}...)
	block, err := aes.NewCipher(c.masterKey)
	if err != nil {
		return nil, err
	}

	block.Encrypt(sessionSalt, sessionSalt)
	return sessionSalt[0:saltLen], nil
}
func (c *Context) generateSessionAuthTag() ([]byte, error) {
	// https://tools.ietf.org/html/rfc3711#appendix-B.3
	// We now show how the auth key is generated.  The input block for AES-
	// CM is generated as above, but using the authentication key label.
	sessionAuthTag := make([]byte, len(c.masterSalt))
	copy(sessionAuthTag, c.masterSalt)

	labelAndIndexOverKdr := []byte{labelAuthenticationTag, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}
	for i, j := len(labelAndIndexOverKdr)-1, len(sessionAuthTag)-1; i >= 0; i, j = i-1, j-1 {
		sessionAuthTag[j] = sessionAuthTag[j] ^ labelAndIndexOverKdr[i]
	}

	// That value is padded and encrypted as above.
	// - We need to do multiple runs at key size (20) is larger then source
	firstRun := append(sessionAuthTag, []byte{0x00, 0x00}...)
	secondRun := append(sessionAuthTag, []byte{0x00, 0x01}...)
	block, err := aes.NewCipher(c.masterKey)
	if err != nil {
		return nil, err
	}

	block.Encrypt(firstRun, firstRun)
	block.Encrypt(secondRun, secondRun)
	return append(firstRun, secondRun[:4]...), nil
}

// Generate IV https://tools.ietf.org/html/rfc3711#section-4.1.1
// where the 128-bit integer value IV SHALL be defined by the SSRC, the
// SRTP packet index i, and the SRTP session salting key k_s, as below.
// - ROC = a 32-bit unsigned rollover counter (ROC), which records how many
// -       times the 16-bit RTP sequence number has been reset to zero after
// -       passing through 65,535
// i = 2^16 * ROC + SEQ
// IV = (salt*2 ^ 16) | (ssrc*2 ^ 64) | (i*2 ^ 16)
func (c *Context) generateCounter(sequenceNumber uint16) []byte {
	counter := make([]byte, 16)

	binary.BigEndian.PutUint32(counter[4:], c.ssrc)
	binary.BigEndian.PutUint32(counter[8:], c.rolloverCounter)
	binary.BigEndian.PutUint32(counter[12:], uint32(sequenceNumber)<<16)

	for i := range c.sessionSalt {
		counter[i] = counter[i] ^ c.sessionSalt[i]
	}

	return counter
}

// https://tools.ietf.org/html/rfc3550#appendix-A.1
func (c *Context) updateRolloverCount(sequenceNumber uint16) {
	if !c.rolloverHasProcessed {
		c.rolloverHasProcessed = true
	} else if sequenceNumber == 0 { // We exactly hit the rollover count

		// Only update rolloverCounter if lastSequenceNumber is greater then maxROCDisorder
		// otherwise we already incremented for disorder
		if c.lastSequenceNumber > maxROCDisorder {
			c.rolloverCounter++
		}
	} else if c.lastSequenceNumber < maxROCDisorder && sequenceNumber > (maxSequenceNumber-maxROCDisorder) {
		// Our last sequence number incremented because we crossed 0, but then our current number was within maxROCDisorder of the max
		// So we fell behind, drop to account for jitter
		c.rolloverCounter--
	} else if sequenceNumber < maxROCDisorder && c.lastSequenceNumber > (maxSequenceNumber-maxROCDisorder) {
		// our current is within a maxROCDisorder of 0
		// and our last sequence number was a high sequence number, increment to account for jitter
		c.rolloverCounter++
	}
	c.lastSequenceNumber = sequenceNumber
}

// DecryptPacket decrypts a RTP packet with an encrypted payload
func (c *Context) DecryptPacket(packet *rtp.Packet) bool {
	c.updateRolloverCount(packet.SequenceNumber)

	stream := cipher.NewCTR(c.block, c.generateCounter(packet.SequenceNumber))
	stream.XORKeyStream(packet.Payload, packet.Payload)

	// TODO remove tags, need to assert value
	packet.Payload = packet.Payload[:len(packet.Payload)-10]

	// Replace payload with decrypted
	packet.Raw = packet.Raw[0:packet.PayloadOffset]
	packet.Raw = append(packet.Raw, packet.Payload...)

	return true
}

func (c *Context) addAuthTag(packet *rtp.Packet) error {
	// https://tools.ietf.org/html/rfc3711#section-4.2
	// In the case of SRTP, M SHALL consist of the Authenticated
	// Portion of the packet (as specified in Figure 1) concatenated with
	// the ROC, M = Authenticated Portion || ROC;
	//
	// The pre-defined authentication transform for SRTP is HMAC-SHA1
	// [RFC2104].  With HMAC-SHA1, the SRTP_PREFIX_LENGTH (Figure 3) SHALL
	// be 0.  For SRTP (respectively SRTCP), the HMAC SHALL be applied to
	// the session authentication key and M as specified above, i.e.,
	// HMAC(k_a, M).  The HMAC output SHALL then be truncated to the n_tag
	// left-most bits.
	// - Authenticated portion of the packet is everything BEFORE MKI
	// - k_a is the session message authentication key
	// - n_tag is the bit-length of the output authentication tag

	mac := hmac.New(sha1.New, c.sessionAuthTag)
	fullPkt, err := packet.Marshal()
	if err != nil {
		panic(err)
	}

	fullPkt = append(fullPkt, make([]byte, 4)...)
	binary.BigEndian.PutUint32(fullPkt[len(fullPkt)-4:], c.rolloverCounter)

	if _, err := mac.Write(fullPkt); err != nil {
		panic(err)
	}

	packet.Payload = append(packet.Payload, mac.Sum(nil)[0:10]...)
	return nil
}

// EncryptPacket Encrypts a SRTP packet in place
func (c *Context) EncryptPacket(packet *rtp.Packet) bool {
	c.updateRolloverCount(packet.SequenceNumber)

	stream := cipher.NewCTR(c.block, c.generateCounter(packet.SequenceNumber))
	stream.XORKeyStream(packet.Payload, packet.Payload)

	if err := c.addAuthTag(packet); err != nil {
		return false
	}

	return true
}
