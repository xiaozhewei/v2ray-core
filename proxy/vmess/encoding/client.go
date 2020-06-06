package encoding

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"hash"
	"hash/fnv"
	"io"
	"os"
	vmessaead "v2ray.com/core/proxy/vmess/aead"

	"golang.org/x/crypto/chacha20poly1305"

	"v2ray.com/core/common"
	"v2ray.com/core/common/bitmask"
	"v2ray.com/core/common/buf"
	"v2ray.com/core/common/crypto"
	"v2ray.com/core/common/dice"
	"v2ray.com/core/common/protocol"
	"v2ray.com/core/common/serial"
	"v2ray.com/core/proxy/vmess"
)

func hashTimestamp(h hash.Hash, t protocol.Timestamp) []byte {
	common.Must2(serial.WriteUint64(h, uint64(t)))
	common.Must2(serial.WriteUint64(h, uint64(t)))
	common.Must2(serial.WriteUint64(h, uint64(t)))
	common.Must2(serial.WriteUint64(h, uint64(t)))
	return h.Sum(nil)
}

// ClientSession stores connection session info for VMess client.
type ClientSession struct {
	idHash          protocol.IDHash
	requestBodyKey  [16]byte
	requestBodyIV   [16]byte
	responseBodyKey [16]byte
	responseBodyIV  [16]byte
	responseReader  io.Reader
	responseHeader  byte

	isAEADRequest bool
}

// NewClientSession creates a new ClientSession.
func NewClientSession(idHash protocol.IDHash) *ClientSession {
	randomBytes := make([]byte, 33) // 16 + 16 + 1
	common.Must2(rand.Read(randomBytes))

	session := &ClientSession{}

	session.isAEADRequest = true

	if vmessexp, vmessexp_found := os.LookupEnv("VMESSAEADEXPERIMENT"); vmessexp_found {
		if vmessexp == "y" {
			session.isAEADRequest = true
		}
		if vmessexp == "n" {
			session.isAEADRequest = false
		}
	}

	if session.isAEADRequest {
		fmt.Println("=======VMESSAEADEXPERIMENT ENABLED========")
	}

	copy(session.requestBodyKey[:], randomBytes[:16])
	copy(session.requestBodyIV[:], randomBytes[16:32])
	session.responseHeader = randomBytes[32]
	if !session.isAEADRequest {
		session.responseBodyKey = md5.Sum(session.requestBodyKey[:])
		session.responseBodyIV = md5.Sum(session.requestBodyIV[:])
	} else {
		BodyKey := sha256.Sum256(session.requestBodyKey[:])
		copy(session.responseBodyKey[:], BodyKey[:16])
		BodyIV := sha256.Sum256(session.requestBodyKey[:])
		copy(session.responseBodyIV[:], BodyIV[:16])
	}

	session.idHash = idHash

	return session
}

func (c *ClientSession) EncodeRequestHeader(header *protocol.RequestHeader, writer io.Writer) error {
	timestamp := protocol.NewTimestampGenerator(protocol.NowTime(), 30)()
	account := header.User.Account.(*vmess.MemoryAccount)
	if !c.isAEADRequest {
		idHash := c.idHash(account.AnyValidID().Bytes())
		common.Must2(serial.WriteUint64(idHash, uint64(timestamp)))
		common.Must2(writer.Write(idHash.Sum(nil)))
	}

	buffer := buf.New()
	defer buffer.Release()

	common.Must(buffer.WriteByte(Version))
	common.Must2(buffer.Write(c.requestBodyIV[:]))
	common.Must2(buffer.Write(c.requestBodyKey[:]))
	common.Must(buffer.WriteByte(c.responseHeader))
	common.Must(buffer.WriteByte(byte(header.Option)))

	padingLen := dice.Roll(16)
	security := byte(padingLen<<4) | byte(header.Security)
	common.Must2(buffer.Write([]byte{security, byte(0), byte(header.Command)}))

	if header.Command != protocol.RequestCommandMux {
		if err := addrParser.WriteAddressPort(buffer, header.Address, header.Port); err != nil {
			return newError("failed to writer address and port").Base(err)
		}
	}

	if padingLen > 0 {
		common.Must2(buffer.ReadFullFrom(rand.Reader, int32(padingLen)))
	}

	{
		fnv1a := fnv.New32a()
		common.Must2(fnv1a.Write(buffer.Bytes()))
		hashBytes := buffer.Extend(int32(fnv1a.Size()))
		fnv1a.Sum(hashBytes[:0])
	}

	if !c.isAEADRequest {
		iv := hashTimestamp(md5.New(), timestamp)
		aesStream := crypto.NewAesEncryptionStream(account.ID.CmdKey(), iv[:])
		aesStream.XORKeyStream(buffer.Bytes(), buffer.Bytes())
		common.Must2(writer.Write(buffer.Bytes()))
	} else {
		var fixedLengthCmdKey [16]byte
		copy(fixedLengthCmdKey[:], account.ID.CmdKey())
		vmessout := vmessaead.SealVMessAEADHeader(fixedLengthCmdKey, buffer.Bytes())
		common.Must2(io.Copy(writer, bytes.NewReader(vmessout)))
	}

	return nil
}

func (c *ClientSession) EncodeRequestBody(request *protocol.RequestHeader, writer io.Writer) buf.Writer {
	var sizeParser crypto.ChunkSizeEncoder = crypto.PlainChunkSizeParser{}
	if request.Option.Has(protocol.RequestOptionChunkMasking) {
		sizeParser = NewShakeSizeParser(c.requestBodyIV[:])
	}
	var padding crypto.PaddingLengthGenerator
	if request.Option.Has(protocol.RequestOptionGlobalPadding) {
		padding = sizeParser.(crypto.PaddingLengthGenerator)
	}

	switch request.Security {
	case protocol.SecurityType_NONE:
		if request.Option.Has(protocol.RequestOptionChunkStream) {
			if request.Command.TransferType() == protocol.TransferTypeStream {
				return crypto.NewChunkStreamWriter(sizeParser, writer)
			}
			auth := &crypto.AEADAuthenticator{
				AEAD:                    new(NoOpAuthenticator),
				NonceGenerator:          crypto.GenerateEmptyBytes(),
				AdditionalDataGenerator: crypto.GenerateEmptyBytes(),
			}
			return crypto.NewAuthenticationWriter(auth, sizeParser, writer, protocol.TransferTypePacket, padding)
		}

		return buf.NewWriter(writer)
	case protocol.SecurityType_LEGACY:
		aesStream := crypto.NewAesEncryptionStream(c.requestBodyKey[:], c.requestBodyIV[:])
		cryptionWriter := crypto.NewCryptionWriter(aesStream, writer)
		if request.Option.Has(protocol.RequestOptionChunkStream) {
			auth := &crypto.AEADAuthenticator{
				AEAD:                    new(FnvAuthenticator),
				NonceGenerator:          crypto.GenerateEmptyBytes(),
				AdditionalDataGenerator: crypto.GenerateEmptyBytes(),
			}
			return crypto.NewAuthenticationWriter(auth, sizeParser, cryptionWriter, request.Command.TransferType(), padding)
		}

		return &buf.SequentialWriter{Writer: cryptionWriter}
	case protocol.SecurityType_AES128_GCM:
		aead := crypto.NewAesGcm(c.requestBodyKey[:])
		auth := &crypto.AEADAuthenticator{
			AEAD:                    aead,
			NonceGenerator:          GenerateChunkNonce(c.requestBodyIV[:], uint32(aead.NonceSize())),
			AdditionalDataGenerator: crypto.GenerateEmptyBytes(),
		}
		return crypto.NewAuthenticationWriter(auth, sizeParser, writer, request.Command.TransferType(), padding)
	case protocol.SecurityType_CHACHA20_POLY1305:
		aead, err := chacha20poly1305.New(GenerateChacha20Poly1305Key(c.requestBodyKey[:]))
		common.Must(err)

		auth := &crypto.AEADAuthenticator{
			AEAD:                    aead,
			NonceGenerator:          GenerateChunkNonce(c.requestBodyIV[:], uint32(aead.NonceSize())),
			AdditionalDataGenerator: crypto.GenerateEmptyBytes(),
		}
		return crypto.NewAuthenticationWriter(auth, sizeParser, writer, request.Command.TransferType(), padding)
	default:
		panic("Unknown security type.")
	}
}

func (c *ClientSession) DecodeResponseHeader(reader io.Reader) (*protocol.ResponseHeader, error) {
	if !c.isAEADRequest {
		aesStream := crypto.NewAesDecryptionStream(c.responseBodyKey[:], c.responseBodyIV[:])
		c.responseReader = crypto.NewCryptionReader(aesStream, reader)
	} else {
		resph := vmessaead.KDF16(c.responseBodyKey[:], "AEAD Resp Header Len Key")
		respi := vmessaead.KDF(c.responseBodyIV[:], "AEAD Resp Header Len IV")[:12]

		aesblock := common.Must2(aes.NewCipher(resph)).(cipher.Block)
		aeadHeader := common.Must2(cipher.NewGCM(aesblock)).(cipher.AEAD)

		var AEADLen [18]byte
		var lenresp int

		var lenrespr uint16

		if _, err := io.ReadFull(reader, AEADLen[:]); err != nil {
			return nil, newError("Unable to Read Header Len").Base(err)
		}
		if AEADLend, err := aeadHeader.Open(nil, respi, AEADLen[:], nil); err != nil {
			return nil, newError("Failed To Decrypt Length").Base(err)
		} else {
			common.Must(binary.Read(bytes.NewReader(AEADLend), binary.BigEndian, &lenrespr))
			lenresp = int(lenrespr)
		}

		resphc := vmessaead.KDF16(c.responseBodyKey[:], "AEAD Resp Header Key")
		respic := vmessaead.KDF(c.responseBodyIV[:], "AEAD Resp Header IV")[:12]

		aesblockc := common.Must2(aes.NewCipher(resphc)).(cipher.Block)
		aeadHeaderc := common.Must2(cipher.NewGCM(aesblockc)).(cipher.AEAD)

		respPayload := make([]byte, lenresp+16)

		if _, err := io.ReadFull(reader, respPayload); err != nil {
			return nil, newError("Unable to Read Header Data").Base(err)
		}

		if AEADData, err := aeadHeaderc.Open(nil, respic, respPayload, nil); err != nil {
			return nil, newError("Failed To Decrypt Payload").Base(err)
		} else {
			c.responseReader = bytes.NewReader(AEADData)
		}
	}

	buffer := buf.StackNew()
	defer buffer.Release()

	if _, err := buffer.ReadFullFrom(c.responseReader, 4); err != nil {
		return nil, newError("failed to read response header").Base(err).AtWarning()
	}

	if buffer.Byte(0) != c.responseHeader {
		return nil, newError("unexpected response header. Expecting ", int(c.responseHeader), " but actually ", int(buffer.Byte(0)))
	}

	header := &protocol.ResponseHeader{
		Option: bitmask.Byte(buffer.Byte(1)),
	}

	if buffer.Byte(2) != 0 {
		cmdID := buffer.Byte(2)
		dataLen := int32(buffer.Byte(3))

		buffer.Clear()
		if _, err := buffer.ReadFullFrom(c.responseReader, dataLen); err != nil {
			return nil, newError("failed to read response command").Base(err)
		}
		command, err := UnmarshalCommand(cmdID, buffer.Bytes())
		if err == nil {
			header.Command = command
		}
	}
	if c.isAEADRequest {
		aesStream := crypto.NewAesDecryptionStream(c.responseBodyKey[:], c.responseBodyIV[:])
		c.responseReader = crypto.NewCryptionReader(aesStream, reader)
	}
	return header, nil
}

func (c *ClientSession) DecodeResponseBody(request *protocol.RequestHeader, reader io.Reader) buf.Reader {
	var sizeParser crypto.ChunkSizeDecoder = crypto.PlainChunkSizeParser{}
	if request.Option.Has(protocol.RequestOptionChunkMasking) {
		sizeParser = NewShakeSizeParser(c.responseBodyIV[:])
	}
	var padding crypto.PaddingLengthGenerator
	if request.Option.Has(protocol.RequestOptionGlobalPadding) {
		padding = sizeParser.(crypto.PaddingLengthGenerator)
	}

	switch request.Security {
	case protocol.SecurityType_NONE:
		if request.Option.Has(protocol.RequestOptionChunkStream) {
			if request.Command.TransferType() == protocol.TransferTypeStream {
				return crypto.NewChunkStreamReader(sizeParser, reader)
			}

			auth := &crypto.AEADAuthenticator{
				AEAD:                    new(NoOpAuthenticator),
				NonceGenerator:          crypto.GenerateEmptyBytes(),
				AdditionalDataGenerator: crypto.GenerateEmptyBytes(),
			}

			return crypto.NewAuthenticationReader(auth, sizeParser, reader, protocol.TransferTypePacket, padding)
		}

		return buf.NewReader(reader)
	case protocol.SecurityType_LEGACY:
		if request.Option.Has(protocol.RequestOptionChunkStream) {
			auth := &crypto.AEADAuthenticator{
				AEAD:                    new(FnvAuthenticator),
				NonceGenerator:          crypto.GenerateEmptyBytes(),
				AdditionalDataGenerator: crypto.GenerateEmptyBytes(),
			}
			return crypto.NewAuthenticationReader(auth, sizeParser, c.responseReader, request.Command.TransferType(), padding)
		}

		return buf.NewReader(c.responseReader)
	case protocol.SecurityType_AES128_GCM:
		aead := crypto.NewAesGcm(c.responseBodyKey[:])

		auth := &crypto.AEADAuthenticator{
			AEAD:                    aead,
			NonceGenerator:          GenerateChunkNonce(c.responseBodyIV[:], uint32(aead.NonceSize())),
			AdditionalDataGenerator: crypto.GenerateEmptyBytes(),
		}
		return crypto.NewAuthenticationReader(auth, sizeParser, reader, request.Command.TransferType(), padding)
	case protocol.SecurityType_CHACHA20_POLY1305:
		aead, _ := chacha20poly1305.New(GenerateChacha20Poly1305Key(c.responseBodyKey[:]))

		auth := &crypto.AEADAuthenticator{
			AEAD:                    aead,
			NonceGenerator:          GenerateChunkNonce(c.responseBodyIV[:], uint32(aead.NonceSize())),
			AdditionalDataGenerator: crypto.GenerateEmptyBytes(),
		}
		return crypto.NewAuthenticationReader(auth, sizeParser, reader, request.Command.TransferType(), padding)
	default:
		panic("Unknown security type.")
	}
}

func GenerateChunkNonce(nonce []byte, size uint32) crypto.BytesGenerator {
	c := append([]byte(nil), nonce...)
	count := uint16(0)
	return func() []byte {
		binary.BigEndian.PutUint16(c, count)
		count++
		return c[:size]
	}
}
