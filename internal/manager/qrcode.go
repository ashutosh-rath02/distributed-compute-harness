package manager

import (
	"fmt"
	"strings"
)

// qrSVG encodes text as a fixed Version 10-L QR symbol. Enrollment URLs
// are bounded well below that version's 271-byte byte-mode capacity. A
// fixed version keeps this small implementation auditable and avoids a
// runtime/CDN dependency in the local-first dashboard.
func qrSVG(text string) ([]byte, error) {
	const version, dataCodewords, ecPerBlock = 10, 274, 18
	if len([]byte(text)) > 271 {
		return nil, fmt.Errorf("enrollment URL is too long for QR code")
	}
	data := qrDataCodewords([]byte(text), dataCodewords)
	all := qrInterleave(data, []int{68, 68, 69, 69}, ecPerBlock)
	m := newQRMatrix(version)
	m.drawFunctionPatterns()
	m.drawCodewords(all)
	m.drawFormatBits(0)

	const border, scale = 4, 6
	dim := (m.size + border*2) * scale
	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" width="%d" height="%d" shape-rendering="crispEdges"><rect width="100%%" height="100%%" fill="white"/><path fill="black" d="`, dim, dim, dim, dim)
	for y := 0; y < m.size; y++ {
		for x := 0; x < m.size; x++ {
			if m.modules[y][x] {
				fmt.Fprintf(&b, "M%d %dh%dv%dh-%dz", (x+border)*scale, (y+border)*scale, scale, scale, scale)
			}
		}
	}
	b.WriteString(`"/></svg>`)
	return []byte(b.String()), nil
}

func qrDataCodewords(payload []byte, capacity int) []byte {
	bits := make([]bool, 0, capacity*8)
	appendBits := func(value, count int) {
		for i := count - 1; i >= 0; i-- {
			bits = append(bits, (value>>i)&1 != 0)
		}
	}
	appendBits(4, 4) // byte mode
	appendBits(len(payload), 16)
	for _, v := range payload {
		appendBits(int(v), 8)
	}
	for i := 0; i < 4 && len(bits) < capacity*8; i++ {
		bits = append(bits, false)
	}
	for len(bits)%8 != 0 {
		bits = append(bits, false)
	}
	out := make([]byte, 0, capacity)
	for i := 0; i < len(bits); i += 8 {
		var v byte
		for j := 0; j < 8; j++ {
			if bits[i+j] {
				v |= 1 << (7 - j)
			}
		}
		out = append(out, v)
	}
	for pad := byte(0xEC); len(out) < capacity; pad ^= 0xEC ^ 0x11 {
		out = append(out, pad)
	}
	return out
}

func qrInterleave(data []byte, blockLens []int, ecLen int) []byte {
	blocks := make([][]byte, len(blockLens))
	ecc := make([][]byte, len(blockLens))
	pos := 0
	for i, n := range blockLens {
		blocks[i] = append([]byte(nil), data[pos:pos+n]...)
		ecc[i] = qrRemainder(blocks[i], ecLen)
		pos += n
	}
	out := make([]byte, 0, len(data)+len(blockLens)*ecLen)
	for i := 0; i < 69; i++ {
		for _, block := range blocks {
			if i < len(block) {
				out = append(out, block[i])
			}
		}
	}
	for i := 0; i < ecLen; i++ {
		for _, block := range ecc {
			out = append(out, block[i])
		}
	}
	return out
}

func qrRemainder(data []byte, degree int) []byte {
	divisor := make([]byte, degree)
	divisor[degree-1] = 1
	root := byte(1)
	for i := 0; i < degree; i++ {
		for j := 0; j < degree; j++ {
			divisor[j] = qrMultiply(divisor[j], root)
			if j+1 < degree {
				divisor[j] ^= divisor[j+1]
			}
		}
		root = qrMultiply(root, 2)
	}
	result := make([]byte, degree)
	for _, b := range data {
		factor := b ^ result[0]
		copy(result, result[1:])
		result[degree-1] = 0
		for i, coef := range divisor {
			result[i] ^= qrMultiply(coef, factor)
		}
	}
	return result
}

func qrMultiply(x, y byte) byte {
	var z byte
	for i := 7; i >= 0; i-- {
		z = (z << 1) ^ ((z >> 7) * 0x1D)
		if (y>>i)&1 != 0 {
			z ^= x
		}
	}
	return z
}

type qrMatrix struct {
	size     int
	modules  [][]bool
	function [][]bool
}

func newQRMatrix(version int) *qrMatrix {
	size := version*4 + 17
	m := &qrMatrix{size: size, modules: make([][]bool, size), function: make([][]bool, size)}
	for i := range size {
		m.modules[i] = make([]bool, size)
		m.function[i] = make([]bool, size)
	}
	return m
}

func (m *qrMatrix) setFunction(x, y int, black bool) {
	if x >= 0 && x < m.size && y >= 0 && y < m.size {
		m.modules[y][x], m.function[y][x] = black, true
	}
}

func (m *qrMatrix) drawFinder(cx, cy int) {
	for dy := -4; dy <= 4; dy++ {
		for dx := -4; dx <= 4; dx++ {
			d := max(abs(dx), abs(dy))
			m.setFunction(cx+dx, cy+dy, d != 2 && d != 4)
		}
	}
}

func (m *qrMatrix) drawAlignment(cx, cy int) {
	for dy := -2; dy <= 2; dy++ {
		for dx := -2; dx <= 2; dx++ {
			m.setFunction(cx+dx, cy+dy, max(abs(dx), abs(dy)) != 1)
		}
	}
}

func (m *qrMatrix) drawFunctionPatterns() {
	for i := 0; i < m.size; i++ {
		m.setFunction(6, i, i%2 == 0)
		m.setFunction(i, 6, i%2 == 0)
	}
	m.drawFinder(3, 3)
	m.drawFinder(m.size-4, 3)
	m.drawFinder(3, m.size-4)
	positions := []int{6, 28, 50}
	for i, y := range positions {
		for j, x := range positions {
			if (i == 0 && j == 0) || (i == 0 && j == len(positions)-1) || (i == len(positions)-1 && j == 0) {
				continue
			}
			m.drawAlignment(x, y)
		}
	}
	m.drawFormatBits(0)
	m.drawVersion()
}

func (m *qrMatrix) drawVersion() {
	bits := qrVersionBits(10)
	for i := 0; i < 18; i++ {
		bit := (bits>>i)&1 != 0
		a, b := m.size-11+i%3, i/3
		m.setFunction(a, b, bit)
		m.setFunction(b, a, bit)
	}
}

func qrVersionBits(version int) int {
	rem := version
	for i := 0; i < 12; i++ {
		rem = (rem << 1) ^ ((rem >> 11) * 0x1F25)
	}
	return version<<12 | rem
}

func (m *qrMatrix) drawFormatBits(mask int) {
	bits := qrFormatBits(mask)
	bit := func(i int) bool { return (bits>>i)&1 != 0 }
	for i := 0; i <= 5; i++ {
		m.setFunction(8, i, bit(i))
	}
	m.setFunction(8, 7, bit(6))
	m.setFunction(8, 8, bit(7))
	m.setFunction(7, 8, bit(8))
	for i := 9; i < 15; i++ {
		m.setFunction(14-i, 8, bit(i))
	}
	for i := 0; i < 8; i++ {
		m.setFunction(m.size-1-i, 8, bit(i))
	}
	for i := 8; i < 15; i++ {
		m.setFunction(8, m.size-15+i, bit(i))
	}
	m.setFunction(8, m.size-8, true)
}

func qrFormatBits(mask int) int {
	data := (1 << 3) | mask // error correction level L
	rem := data
	for i := 0; i < 10; i++ {
		rem = (rem << 1) ^ ((rem >> 9) * 0x537)
	}
	return (data<<10 | rem) ^ 0x5412
}

func (m *qrMatrix) drawCodewords(data []byte) {
	bitIndex, upward := 0, true
	for right := m.size - 1; right >= 1; right -= 2 {
		if right == 6 {
			right--
		}
		for vert := 0; vert < m.size; vert++ {
			y := vert
			if upward {
				y = m.size - 1 - vert
			}
			for j := 0; j < 2; j++ {
				x := right - j
				if m.function[y][x] {
					continue
				}
				black := false
				if bitIndex < len(data)*8 {
					black = (data[bitIndex>>3]>>(7-(bitIndex&7)))&1 != 0
				}
				m.modules[y][x] = black != ((x+y)%2 == 0) // mask 0
				bitIndex++
			}
		}
		upward = !upward
	}
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}
