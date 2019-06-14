package handshake

import (
	"crypto"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"

	"github.com/lucas-clemente/quic-go/internal/protocol"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

type mockCipherSuite struct{}

var _ cipherSuite = &mockCipherSuite{}

func (c *mockCipherSuite) Hash() crypto.Hash { return crypto.SHA256 }
func (c *mockCipherSuite) KeyLen() int       { return 16 }
func (c *mockCipherSuite) IVLen() int        { return 12 }
func (c *mockCipherSuite) AEAD(key, _ []byte) cipher.AEAD {
	block, err := aes.NewCipher(key)
	Expect(err).ToNot(HaveOccurred())
	gcm, err := cipher.NewGCM(block)
	Expect(err).ToNot(HaveOccurred())
	return gcm
}

var _ = Describe("Updatable AEAD", func() {
	getPeers := func() (client, server *updatableAEAD) {
		trafficSecret1 := make([]byte, 16)
		trafficSecret2 := make([]byte, 16)
		rand.Read(trafficSecret1)
		rand.Read(trafficSecret2)

		client = newUpdatableAEAD()
		server = newUpdatableAEAD()
		client.SetReadKey(&mockCipherSuite{}, trafficSecret2)
		client.SetWriteKey(&mockCipherSuite{}, trafficSecret1)
		server.SetReadKey(&mockCipherSuite{}, trafficSecret1)
		server.SetWriteKey(&mockCipherSuite{}, trafficSecret2)
		return
	}

	Context("header protection", func() {
		It("encrypts and decrypts the header", func() {
			server, client := getPeers()
			var lastFiveBitsDifferent int
			for i := 0; i < 100; i++ {
				sample := make([]byte, 16)
				rand.Read(sample)
				header := []byte{0xb5, 1, 2, 3, 4, 5, 6, 7, 8, 0xde, 0xad, 0xbe, 0xef}
				client.EncryptHeader(sample, &header[0], header[9:13])
				if header[0]&0x1f != 0xb5&0x1f {
					lastFiveBitsDifferent++
				}
				Expect(header[0] & 0xe0).To(Equal(byte(0xb5 & 0xe0)))
				Expect(header[1:9]).To(Equal([]byte{1, 2, 3, 4, 5, 6, 7, 8}))
				Expect(header[9:13]).ToNot(Equal([]byte{0xde, 0xad, 0xbe, 0xef}))
				server.DecryptHeader(sample, &header[0], header[9:13])
				Expect(header).To(Equal([]byte{0xb5, 1, 2, 3, 4, 5, 6, 7, 8, 0xde, 0xad, 0xbe, 0xef}))
			}
			Expect(lastFiveBitsDifferent).To(BeNumerically(">", 75))
		})
	})

	Context("message encryption", func() {
		var msg, ad []byte
		var server, client *updatableAEAD

		BeforeEach(func() {
			server, client = getPeers()
			msg = []byte("Lorem ipsum dolor sit amet, consectetur adipiscing elit, sed do eiusmod tempor incididunt ut labore et dolore magna aliqua.")
			ad = []byte("Donec in velit neque.")
		})

		It("encrypts and decrypts a message", func() {
			encrypted := server.Seal(nil, msg, 0x1337, ad)
			opened, err := client.Open(nil, encrypted, 0x1337, protocol.KeyPhaseZero, ad)
			Expect(err).ToNot(HaveOccurred())
			Expect(opened).To(Equal(msg))
		})

		It("fails to open a message if the associated data is not the same", func() {
			encrypted := client.Seal(nil, msg, 0x1337, ad)
			_, err := server.Open(nil, encrypted, 0x1337, protocol.KeyPhaseZero, []byte("wrong ad"))
			Expect(err).To(MatchError(ErrDecryptionFailed))
		})

		It("fails to open a message if the packet number is not the same", func() {
			encrypted := server.Seal(nil, msg, 0x1337, ad)
			_, err := client.Open(nil, encrypted, 0x42, protocol.KeyPhaseZero, ad)
			Expect(err).To(MatchError(ErrDecryptionFailed))
		})

		Context("key updates", func() {
			It("updates keys", func() {
				Expect(server.KeyPhase()).To(Equal(protocol.KeyPhaseZero))
				encrypted0 := server.Seal(nil, msg, 0x1337, ad)
				server.rollKeys()
				Expect(server.KeyPhase()).To(Equal(protocol.KeyPhaseOne))
				encrypted1 := server.Seal(nil, msg, 0x1337, ad)
				Expect(encrypted0).ToNot(Equal(encrypted1))
				// expect opening to fail. The client didn't roll keys yet
				_, err := client.Open(nil, encrypted1, 0x1337, protocol.KeyPhaseZero, ad)
				Expect(err).To(MatchError(ErrDecryptionFailed))
				client.rollKeys()
				decrypted, err := client.Open(nil, encrypted1, 0x1337, protocol.KeyPhaseOne, ad)
				Expect(err).ToNot(HaveOccurred())
				Expect(decrypted).To(Equal(msg))
			})

			It("updates the keys when receiving a packet with the next key phase", func() {
				// receive the first packet at key phase zero
				encrypted0 := client.Seal(nil, msg, 0x42, ad)
				decrypted, err := server.Open(nil, encrypted0, 0x42, protocol.KeyPhaseZero, ad)
				Expect(err).ToNot(HaveOccurred())
				Expect(decrypted).To(Equal(msg))
				// send one packet at key phase zero
				Expect(server.KeyPhase()).To(Equal(protocol.KeyPhaseZero))
				_ = server.Seal(nil, msg, 0x1, ad)
				// now received a message at key phase one
				client.rollKeys()
				encrypted1 := client.Seal(nil, msg, 0x43, ad)
				decrypted, err = server.Open(nil, encrypted1, 0x43, protocol.KeyPhaseOne, ad)
				Expect(err).ToNot(HaveOccurred())
				Expect(decrypted).To(Equal(msg))
				Expect(server.KeyPhase()).To(Equal(protocol.KeyPhaseOne))
			})

			It("opens a reordered packet with the old keys after an update", func() {
				encrypted01 := client.Seal(nil, msg, 0x42, ad)
				encrypted02 := client.Seal(nil, msg, 0x43, ad)
				// receive the first packet with key phase 0
				_, err := server.Open(nil, encrypted01, 0x42, protocol.KeyPhaseZero, ad)
				Expect(err).ToNot(HaveOccurred())
				// send one packet at key phase zero
				_ = server.Seal(nil, msg, 0x1, ad)
				// now receive a packet with key phase 1
				client.rollKeys()
				encrypted1 := client.Seal(nil, msg, 0x44, ad)
				Expect(server.KeyPhase()).To(Equal(protocol.KeyPhaseZero))
				_, err = server.Open(nil, encrypted1, 0x44, protocol.KeyPhaseOne, ad)
				Expect(err).ToNot(HaveOccurred())
				Expect(server.KeyPhase()).To(Equal(protocol.KeyPhaseOne))
				// now receive a reordered packet with key phase 0
				decrypted, err := server.Open(nil, encrypted02, 0x43, protocol.KeyPhaseZero, ad)
				Expect(err).ToNot(HaveOccurred())
				Expect(decrypted).To(Equal(msg))
				Expect(server.KeyPhase()).To(Equal(protocol.KeyPhaseOne))
			})

			It("errors when the peer starts with key phase 1", func() {
				client.rollKeys()
				encrypted := client.Seal(nil, msg, 0x1337, ad)
				_, err := server.Open(nil, encrypted, 0x1337, protocol.KeyPhaseOne, ad)
				Expect(err).To(MatchError("PROTOCOL_VIOLATION: wrong initial keyphase"))
			})

			It("errors when the peer updates keys too frequently", func() {
				// receive the first packet at key phase zero
				encrypted0 := client.Seal(nil, msg, 0x42, ad)
				_, err := server.Open(nil, encrypted0, 0x42, protocol.KeyPhaseZero, ad)
				Expect(err).ToNot(HaveOccurred())
				// now receive a packet at key phase one, before having sent any packets
				client.rollKeys()
				encrypted1 := client.Seal(nil, msg, 0x42, ad)
				_, err = server.Open(nil, encrypted1, 0x42, protocol.KeyPhaseOne, ad)
				Expect(err).To(MatchError("PROTOCOL_VIOLATION: keys updated too quickly"))
			})
		})
	})
})
