package main

import (
	"context"
	"encoding/binary"
	"fmt"
	"github.com/chromedp/chromedp" // ✨ 引入无头浏览器库
	"log"
	"math"
	"math/rand"
	"net/http"
	"regexp"
	"strings"
	"time"
)

func ExtractBuiltinDouyinLiveURL(text string) (string, error) {
	re := regexp.MustCompile(`https?://v\.douyin\.com/[a-zA-Z0-9]+/?`)
	shortURL := re.FindString(text)

	if shortURL == "" {
		return "", fmt.Errorf("未在文本中找到抖音分享短链接")
	}
	log.Printf("\n🔍 [解析引擎] 提取到纯净短链接: %s\n", shortURL)

	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.Flag("headless", true),
		chromedp.Flag("disable-blink-features", "AutomationControlled"),
		chromedp.Flag("disable-extensions", true),
		chromedp.Flag("mute-audio", true),
		chromedp.UserAgent("Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"),
	)
	allocCtx, cancel := chromedp.NewExecAllocator(context.Background(), opts...)
	defer cancel()

	ctx, cancel2 := chromedp.NewContext(allocCtx)
	defer cancel2()

	ctx, cancel3 := context.WithTimeout(ctx, 15*time.Second)
	defer cancel3()

	var finalURL string
	var htmlContent string

	log.Println("⏳ [解析引擎] 正在启动无头 Chrome 进行深度穿透 (最长等待15秒)...")
	err := chromedp.Run(ctx,
		chromedp.Navigate(shortURL),
		chromedp.WaitReady("body"),
		chromedp.Sleep(3*time.Second), // 等待 JS 充分执行重定向
		chromedp.Location(&finalURL),
		chromedp.OuterHTML("html", &htmlContent),
	)

	if err == nil {
		if strings.Contains(finalURL, "douyin.com/user/") {
			return "", fmt.Errorf("解析失败: 该主播当前未开播 (重定向至个人主页)")
		}

		idRe := regexp.MustCompile(`live\.douyin\.com/(\d+)`)
		urlMatches := idRe.FindStringSubmatch(finalURL)
		if len(urlMatches) > 1 {
			idStr := urlMatches[1]
			if len(idStr) < 18 {
				return fmt.Sprintf("https://live.douyin.com/%s", idStr), nil
			}
		}
		webRid := extractBuiltinWebRid(htmlContent)
		if webRid != "" {
			return fmt.Sprintf("https://live.douyin.com/%s", webRid), nil
		}
	} else {
		log.Printf("err: %v", err)
		log.Printf("⚠️ [解析引擎] 无头浏览器超时或未安装，触发底层 HTTP 拦截器保底...")
	}

	fallbackClient := &http.Client{
		Timeout: 10 * time.Second,
	}
	req, _ := http.NewRequest("GET", shortURL, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")

	resp, fallbackErr := fallbackClient.Do(req)
	if fallbackErr == nil {
		defer resp.Body.Close()
		resolvedURL := resp.Request.URL.String()

		if strings.Contains(resolvedURL, "douyin.com/user/") {
			return "", fmt.Errorf("解析失败: 该主播当前未开播 (重定向至个人主页)")
		}

		longIDRe := regexp.MustCompile(`\d{18,20}`)
		longID := longIDRe.FindString(resolvedURL)
		if longID != "" {
			return fmt.Sprintf("https://live.douyin.com/%s", longID), nil
		}
		return strings.Split(resolvedURL, "?")[0], nil
	}

	return "", fmt.Errorf("双重解析方案均未拿到有效房间号，可能是网络受限或滑块拦截")
}

// extractBuiltinWebRid 使用正则暴力在返回的 HTML 数据中筛查包含真实房间号的配置段落
func extractBuiltinWebRid(html string) string {
	patterns := []string{
		`"web_rid"\s*:\s*"(\d+)"`,
		`\\"web_rid\\"\s*:\s*\\"(\d+)\\"`,
		`%22web_rid%22%3A%22(\d+)%22`,
		`"short_id"\s*:\s*"(\d+)"`,
		`\\"short_id\\"\s*:\s*\\"(\d+)\\"`,
		`%22short_id%22%3A%22(\d+)%22`,
		`"web_rid"\s*:\s*(\d+)`,
		`"short_id"\s*:\s*(\d+)`,
	}

	for _, p := range patterns {
		re := regexp.MustCompile(p)
		matches := re.FindAllStringSubmatch(html, -1)
		for _, match := range matches {
			idStr := match[1]
			if len(idStr) > 4 && len(idStr) < 18 {
				return idStr
			}
		}
	}
	return ""
}

// ==========================================
// 核心加密算法复刻 (SM3, RC4, a_bogus)
// ==========================================

// builtinRC4Encrypt 实现标准的 RC4 对称加密方法，用于接口所需的 UserAgent 加密流程
func builtinRC4Encrypt(plaintext, key string) string {
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

// BuiltinSM3 SM3 散列算法结构体定义
type BuiltinSM3 struct {
	reg   []uint32
	chunk []byte
	size  uint64
}

// NewBuiltinSM3 初始化一个满足中国国家密码局算法标准的 SM3 散列计算器
func NewBuiltinSM3() *BuiltinSM3 {
	s := &BuiltinSM3{}
	s.Reset()
	return s
}

// Reset 清空 SM3 计算器的当前上下文状态和内部数据块缓存，重置回初始哈希常量
func (s *BuiltinSM3) Reset() {
	s.reg = []uint32{
		1937774191, 1226093241, 388252375, 3666478592,
		2842636476, 372324522, 3817729613, 2969243214,
	}
	s.chunk = []byte{}
	s.size = 0
}

// leftRotate 执行 SM3 算法中需要的 32 位无符号整型循环左移按位操作
func (s *BuiltinSM3) leftRotate(x uint32, n int) uint32 {
	n &= 0x1f
	if n == 0 {
		return x
	}
	return (x << n) | (x >> (32 - n))
}

// getT 返回对应运算轮次的常数项，属于 SM3 算法标准规范定义的一环
func (s *BuiltinSM3) getT(j int) uint32 {
	if j < 16 {
		return 2043430169
	}
	return 2055708042
}

// ff 执行 SM3 规定的布尔逻辑计算组合函数之一，取决于执行轮次 j 的区间
func (s *BuiltinSM3) ff(j int, x, y, z uint32) uint32 {
	if j < 16 {
		return x ^ y ^ z
	}
	return (x & y) | (x & z) | (y & z)
}

// gg 执行 SM3 规定的布尔逻辑计算组合函数之二，负责后续 48 轮次的数据混淆
func (s *BuiltinSM3) gg(j int, x, y, z uint32) uint32 {
	if j < 16 {
		return x ^ y ^ z
	}
	return (x & y) | (^x & z)
}

// compress 完成对传入的 512 bit (64 Byte) 的单块数据进行消息扩展并注入八位寄存器内
func (s *BuiltinSM3) compress(data []byte) {
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

// Write 满足 hash.Hash 接口定义，持续吞并字符串并放入缓存器触发增量解算
func (s *BuiltinSM3) Write(data string) {
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

// Sum 封装收尾运算，执行数据填充标准 (Bit 1 随后 0 以及长度字段) 输出 32 字节哈希值
func (s *BuiltinSM3) Sum() []byte {
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

// builtinResultEncrypt 依靠逆向取得的前端特制混淆表，对给出的密文数据进行类 Base64 的私有编码替换
func builtinResultEncrypt(longStr, num string) string {
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

// generBuiltinRandom 生成请求校验签名时所需的前置随机噪声比特位组合
func generBuiltinRandom(randomNum int, option []int) []int {
	byte1 := randomNum & 255
	byte2 := (randomNum >> 8) & 255
	return []int{
		(byte1 & 170) | (option[0] & 85),
		(byte1 & 85) | (option[0] & 170),
		(byte2 & 170) | (option[1] & 85),
		(byte2 & 85) | (option[1] & 170),
	}
}

// generateBuiltinRandomStr 根据上述的噪声组合规则，通过时间种子转换得到完全不规律的 ASCII 字节前缀序列
func generateBuiltinRandomStr() string {
	r1 := rand.Float64()
	r2 := rand.Float64()
	r3 := rand.Float64()

	var bytes []int
	bytes = append(bytes, generBuiltinRandom(int(r1*10000), []int{3, 45})...)
	bytes = append(bytes, generBuiltinRandom(int(r2*10000), []int{1, 0})...)
	bytes = append(bytes, generBuiltinRandom(int(r3*10000), []int{1, 5})...)

	var sb strings.Builder
	for _, b := range bytes {
		sb.WriteByte(byte(b))
	}
	return sb.String()
}

// builtinGenerateABogus 负责合并各层算法完成 a_bogus 参数的终极推导以突破某音接口抓取封锁限制
func builtinGenerateABogus(params, userAgent string) string {
	windowEnvStr := "1920|1080|1920|1040|0|30|0|0|1872|92|1920|1040|1857|92|1|24|Win32"
	suffix := "cus"
	arguments := []int{0, 1, 14}

	sm3 := NewBuiltinSM3()
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
	uaEnc := builtinRC4Encrypt(userAgent, uaKey)
	uaB64 := builtinResultEncrypt(uaEnc, "s3")
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

	prefix := generateBuiltinRandomStr()
	body := builtinRC4Encrypt(string(bb), string([]byte{121}))
	return builtinResultEncrypt(prefix+body, "s4") + "="
}

// ==========================================
// 🚀 平台抓取实现
// ==========================================

// ---------------- Douyin ----------------
