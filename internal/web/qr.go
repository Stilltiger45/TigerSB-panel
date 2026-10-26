package web

import (
	"bytes"
	"fmt"

	"rsc.io/qr"
)

// qrSVG draws text as a QR code: one path of unit squares with a quiet zone.
func qrSVG(text string) ([]byte, error) {
	code, err := qr.Encode(text, qr.L)
	if err != nil {
		return nil, err
	}
	const quiet = 4
	n := code.Size + 2*quiet
	var b bytes.Buffer
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" shape-rendering="crispEdges">`, n, n)
	fmt.Fprintf(&b, `<rect width="%d" height="%d" fill="#fff"/><path fill="#000" d="`, n, n)
	for y := 0; y < code.Size; y++ {
		for x := 0; x < code.Size; x++ {
			if code.Black(x, y) {
				fmt.Fprintf(&b, "M%d %dh1v1h-1z", x+quiet, y+quiet)
			}
		}
	}
	b.WriteString(`"/></svg>`)
	return b.Bytes(), nil
}
