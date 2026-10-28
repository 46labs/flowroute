package server

import (
	"strings"
	"unicode/utf16"
)

// GSM 03.38 default alphabet and its extension table, whose characters
// take two septets.
const (
	gsmBasic = "@£$¥èéùìòÇ\nØø\rÅåΔ_ΦΓΛΩΠΨΣΘΞÆæßÉ !\"#¤%&'()*+,-./0123456789:;<=>?" +
		"¡ABCDEFGHIJKLMNOPQRSTUVWXYZÄÖÑÜ§¿abcdefghijklmnopqrstuvwxyzäöñüà"
	gsmExtension = "\f^{}\\[~]|€"
)

// Encodings as SMPP data_coding values.
const (
	encodingGSM7 = 0
	encodingUCS2 = 8
)

// segmentCount returns the number of SMS segments body needs and its
// encoding: GSM-7 at 160 septets, or 153 per part when concatenated, else
// UCS-2 at 70 code units, or 67 per part. A character's units never split
// across parts, so escape sequences and surrogate pairs stay whole.
func segmentCount(body string) (int, int) {
	units := make([]int, 0, len(body))
	for _, r := range body {
		switch {
		case strings.ContainsRune(gsmBasic, r):
			units = append(units, 1)
		case strings.ContainsRune(gsmExtension, r):
			units = append(units, 2)
		default:
			units = units[:0]
			for _, r := range body {
				units = append(units, utf16.RuneLen(r))
			}
			return parts(units, 70, 67), encodingUCS2
		}
	}
	return parts(units, 160, 153), encodingGSM7
}

func parts(units []int, single, multi int) int {
	total := 0
	for _, u := range units {
		total += u
	}
	if total <= single {
		return 1
	}
	n, used := 1, 0
	for _, u := range units {
		if used+u > multi {
			n, used = n+1, 0
		}
		used += u
	}
	return n
}
