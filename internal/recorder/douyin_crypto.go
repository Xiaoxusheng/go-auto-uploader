// Package recorder — 抖音接口 a_bogus / SM3 / RC4 纯算法（无网络依赖）。
package recorder

import (
	"encoding/binary"
	"math"
	"math/rand"
	"strings"
	"time"
)

// RC4Encrypt 标准 RC4，用于 UA 加密。
func RC4Encrypt(plaintext, key string) string {
	s := make([]int, 256)
	for i := 0; i < 256; i++ {
		s[i] = i
	}
	j := 0
	for i := 0; i < 256; i++ {
		j = (j + s[i] + int(key[i%len(key)])) % 256
		s[i], s[j] = s[j], s[i]
	}
	i := 0
	j = 0
	res := make([]byte, len(plaintext))
	for k := 0; k < len(plaintext); k++ {
		i = (i + 1) % 256
		j = (j + s[i]) % 256
		s[i], s[j] = s[j], s[i]
		t := (s[i] + s[j]) % 256
		res[k] = byte(int(plaintext[k]) ^ s[t])
	}
	return string(res)
}

// SM3 国密散列。
type SM3 struct {
	reg   []uint32
	chunk []byte
	size  uint64
}

// NewSM3 创建 SM3。
func NewSM3() *SM3 {
	s := &SM3{}
	s.Reset()
	return s
}

// Reset 重置状态。
func (s *SM3) Reset() {
	s.reg = []uint32{
		1937774191, 1226093241, 388252375, 3666478592,
		2842636476, 372324522, 3817729613, 2969243214,
	}
	s.chunk = []byte{}
	s.size = 0
}

func (s *SM3) leftRotate(x uint32, n int) uint32 {
	n &= 0x1f
	if n == 0 {
		return x
	}
	return (x << n) | (x >> (32 - n))
}

func (s *SM3) getT(j int) uint32 {
	if j < 16 {
		return 2043430169
	}
	return 2055708042
}

func (s *SM3) ff(j int, x, y, z uint32) uint32 {
	if j < 16 {
		return x ^ y ^ z
	}
	return (x & y) | (x & z) | (y & z)
}

func (s *SM3) gg(j int, x, y, z uint32) uint32 {
	if j < 16 {
		return x ^ y ^ z
	}
	return (x & y) | (^x & z)
}

func (s *SM3) compress(data []byte) {
	w := make([]uint32, 132)
	for t := 0; t < 16; t++ {
		w[t] = binary.BigEndian.Uint32(data[4*t : 4*t+4])
	}
	for j := 16; j < 68; j++ {
		a := w[j-16] ^ w[j-9] ^ s.leftRotate(w[j-3], 15)
		w[j] = a ^ s.leftRotate(a, 15) ^ s.leftRotate(a, 23) ^ s.leftRotate(w[j-13], 7) ^ w[j-6]
	}
	for j := 0; j < 64; j++ {
		w[j+68] = w[j] ^ w[j+4]
	}
	a, b, c, d, e, f, g, h := s.reg[0], s.reg[1], s.reg[2], s.reg[3], s.reg[4], s.reg[5], s.reg[6], s.reg[7]
	for j := 0; j < 64; j++ {
		ss1 := s.leftRotate((s.leftRotate(a, 12) + e + s.leftRotate(s.getT(j), j)), 7)
		ss2 := ss1 ^ s.leftRotate(a, 12)
		tt1 := s.ff(j, a, b, c) + d + ss2 + w[j+68]
		tt2 := s.gg(j, e, f, g) + h + ss1 + w[j]
		d = c
		c = s.leftRotate(b, 9)
		b = a
		a = tt1
		h = g
		g = s.leftRotate(f, 19)
		f = e
		e = tt2 ^ s.leftRotate(tt2, 9) ^ s.leftRotate(tt2, 17)
	}
	s.reg[0] ^= a
	s.reg[1] ^= b
	s.reg[2] ^= c
	s.reg[3] ^= d
	s.reg[4] ^= e
	s.reg[5] ^= f
	s.reg[6] ^= g
	s.reg[7] ^= h
}

// Write 增量写入。
func (s *SM3) Write(data string) {
	b := []byte(data)
	s.size += uint64(len(b))
	f := 64 - len(s.chunk)
	if len(b) < f {
		s.chunk = append(s.chunk, b...)
	} else {
		s.chunk = append(s.chunk, b[:f]...)
		for len(s.chunk) >= 64 {
			s.compress(s.chunk)
			b = b[f:]
			if len(b) < 64 {
				s.chunk = b
				break
			}
			s.chunk = b[:64]
			f = 64
		}
	}
}

// Sum 输出 32 字节摘要。
func (s *SM3) Sum() []byte {
	bitLength := s.size * 8
	s.chunk = append(s.chunk, 0x80)
	for (len(s.chunk)+8)%64 != 0 {
		s.chunk = append(s.chunk, 0)
	}
	lenBytes := make([]byte, 8)
	binary.BigEndian.PutUint64(lenBytes, bitLength)
	s.chunk = append(s.chunk, lenBytes...)
	for i := 0; i < len(s.chunk); i += 64 {
		s.compress(s.chunk[i : i+64])
	}
	res := make([]byte, 32)
	for i := 0; i < 8; i++ {
		binary.BigEndian.PutUint32(res[4*i:], s.reg[i])
	}
	s.Reset()
	return res
}

func resultEncrypt(longStr, num string) string {
	encodingTables := map[string]string{
		"s0": "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/=",
		"s1": "Dkdpgh4ZKsQB80/Mfvw36XI1R25+WUAlEi7NLboqYTOPuzmFjJnryx9HVGcaStCe=",
		"s2": "Dkdpgh4ZKsQB80/Mfvw36XI1R25-WUAlEi7NLboqYTOPuzmFjJnryx9HVGcaStCe=",
		"s3": "ckdp1h4ZKsUB80/Mfvw36XIgR25+WQAlEi7NLboqYTOPuzmFjJnryx9HVGDaStCe",
		"s4": "Dkdpgh2ZmsQB80/MfvV36XI1R45-WUAlEixNLwoqYTOPuzKFjJnry79HbGcaStCe",
	}
	table := encodingTables[num]
	masks := []int{16515072, 258048, 4032, 63}
	shifts := []int{18, 12, 6, 0}
	var res strings.Builder
	roundNum := 0
	getLongInt := func(round int, s string) int {
		idx := round * 3
		var ch1, ch2, ch3 int
		if idx < len(s) {
			ch1 = int(s[idx])
		}
		if idx+1 < len(s) {
			ch2 = int(s[idx+1])
		}
		if idx+2 < len(s) {
			ch3 = int(s[idx+2])
		}
		return (ch1 << 16) | (ch2 << 8) | ch3
	}
	longInt := getLongInt(roundNum, longStr)
	totalChars := int(math.Ceil(float64(len(longStr)) / 3.0 * 4.0))
	for i := 0; i < totalChars; i++ {
		if i/4 != roundNum {
			roundNum++
			longInt = getLongInt(roundNum, longStr)
		}
		index := i % 4
		charIndex := (longInt & masks[index]) >> shifts[index]
		res.WriteByte(table[charIndex])
	}
	return res.String()
}

func generRandom(randomNum int, option []int) []int {
	byte1 := randomNum & 255
	byte2 := (randomNum >> 8) & 255
	return []int{
		(byte1 & 170) | (option[0] & 85),
		(byte1 & 85) | (option[0] & 170),
		(byte2 & 170) | (option[1] & 85),
		(byte2 & 85) | (option[1] & 170),
	}
}

func generateRandomStr() string {
	r1 := rand.Float64()
	r2 := rand.Float64()
	r3 := rand.Float64()

	var bytes []int
	bytes = append(bytes, generRandom(int(r1*10000), []int{3, 45})...)
	bytes = append(bytes, generRandom(int(r2*10000), []int{1, 0})...)
	bytes = append(bytes, generRandom(int(r3*10000), []int{1, 5})...)

	var sb strings.Builder
	for _, b := range bytes {
		sb.WriteByte(byte(b))
	}
	return sb.String()
}

// GenerateABogus 生成抖音 a_bogus 签名。
func GenerateABogus(params, userAgent string) string {
	windowEnvStr := "1920|1080|1920|1040|0|30|0|0|1872|92|1920|1040|1857|92|1|24|Win32"
	suffix := "cus"
	arguments := []int{0, 1, 14}

	sm3 := NewSM3()
	startTime := int(time.Now().UnixNano() / 1e6)

	sm3.Write(params + suffix)
	hash1 := string(sm3.Sum())
	sm3.Write(hash1)
	urlSearchParamsList := sm3.Sum()

	sm3.Write(suffix)
	hash2 := string(sm3.Sum())
	sm3.Write(hash2)
	cus := sm3.Sum()

	uaKey := string([]byte{0, 1, 14})
	uaEnc := RC4Encrypt(userAgent, uaKey)
	uaB64 := resultEncrypt(uaEnc, "s3")
	sm3.Write(uaB64)
	uaHash := sm3.Sum()

	b := make(map[int]int)
	b[8] = 3
	b[10] = startTime + 100
	b[16] = startTime
	b[18] = 44

	splitToBytes := func(num int) []int {
		return []int{(num >> 24) & 255, (num >> 16) & 255, (num >> 8) & 255, num & 255}
	}

	stBytes := splitToBytes(b[16])
	b[20], b[21], b[22], b[23] = stBytes[0], stBytes[1], stBytes[2], stBytes[3]
	b[24] = (b[16] >> 32) & 255
	b[25] = (b[16] >> 40) & 255

	arg0 := splitToBytes(arguments[0])
	b[26], b[27], b[28], b[29] = arg0[0], arg0[1], arg0[2], arg0[3]
	b[30] = (arguments[1] >> 8) & 255
	b[31] = arguments[1] & 255
	arg1 := splitToBytes(arguments[1])
	b[32], b[33] = arg1[0], arg1[1]
	arg2 := splitToBytes(arguments[2])
	b[34], b[35], b[36], b[37] = arg2[0], arg2[1], arg2[2], arg2[3]

	b[38] = int(urlSearchParamsList[21])
	b[39] = int(urlSearchParamsList[22])
	b[40] = int(cus[21])
	b[41] = int(cus[22])
	b[42] = int(uaHash[23])
	b[43] = int(uaHash[24])

	etBytes := splitToBytes(b[10])
	b[44], b[45], b[46], b[47] = etBytes[0], etBytes[1], etBytes[2], etBytes[3]
	b[48] = b[8]
	b[49] = (b[10] >> 32) & 255
	b[50] = (b[10] >> 40) & 255

	pageId := 110624
	b[51] = pageId
	pIdBytes := splitToBytes(pageId)
	b[52], b[53], b[54], b[55] = pIdBytes[0], pIdBytes[1], pIdBytes[2], pIdBytes[3]

	aid := 6383
	b[56] = aid
	b[57] = aid & 255
	b[58] = (aid >> 8) & 255
	b[59] = (aid >> 16) & 255
	b[60] = (aid >> 24) & 255

	winEnvList := []byte(windowEnvStr)
	b[64] = len(winEnvList)
	b[65] = b[64] & 255
	b[66] = (b[64] >> 8) & 255
	b[69], b[70], b[71] = 0, 0, 0

	xorSum := b[18] ^ b[20] ^ b[26] ^ b[30] ^ b[38] ^ b[40] ^ b[42] ^ b[21] ^ b[27] ^ b[31] ^
		b[35] ^ b[39] ^ b[41] ^ b[43] ^ b[22] ^ b[28] ^ b[32] ^ b[36] ^ b[23] ^ b[29] ^
		b[33] ^ b[37] ^ b[44] ^ b[45] ^ b[46] ^ b[47] ^ b[48] ^ b[49] ^ b[50] ^ b[24] ^
		b[25] ^ b[52] ^ b[53] ^ b[54] ^ b[55] ^ b[57] ^ b[58] ^ b[59] ^ b[60] ^ b[65] ^
		b[66] ^ b[70] ^ b[71]
	b[72] = xorSum

	var bb []byte
	indices := []int{
		18, 20, 52, 26, 30, 34, 58, 38, 40, 53, 42, 21,
		27, 54, 55, 31, 35, 57, 39, 41, 43, 22, 28, 32,
		60, 36, 23, 29, 33, 37, 44, 45, 59, 46, 47, 48,
		49, 50, 24, 25, 65, 66, 70, 71,
	}
	for _, idx := range indices {
		bb = append(bb, byte(b[idx]))
	}
	bb = append(bb, winEnvList...)
	bb = append(bb, byte(b[72]))

	prefix := generateRandomStr()
	body := RC4Encrypt(string(bb), string([]byte{121}))
	return resultEncrypt(prefix+body, "s4") + "="
}
