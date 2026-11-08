package telegram

import (
	"strings"
	"unicode/utf8"
)

// utf16Units is the number of UTF-16 code units one rune costs in Telegram's
// send limit: an astral-plane rune arrives as a surrogate pair.
func utf16Units(r rune) int {
	if r >= 0x10000 {
		return 2
	}
	return 1
}

// utf16StringLen counts the UTF-16 code units Telegram charges for text.
func utf16StringLen(text string) int {
	units := 0
	for _, r := range text {
		units += utf16Units(r)
	}
	return units
}

// chunk splits text into messages of at most maxMessageUTF16CodeUnits
// UTF-16 code units. Splits happen only at rune boundaries and prefer a
// line break over a mid-line split; a single line longer than the limit
// hard-splits at a rune boundary. A text within the limit returns one chunk
// unchanged, and the chunks always reassemble into the input.
func chunk(text string) []string {
	if utf16StringLen(text) <= maxMessageUTF16CodeUnits {
		return []string{text}
	}

	var chunks []string
	start := 0
	for start < len(text) {
		end := chunkBudget(text, start)
		if end < len(text) {
			// The remainder overflows, so this chunk needs a hard cut: prefer
			// ending it on a line break inside the fit. A remainder that fits
			// whole is never re-split.
			if nl := strings.LastIndexByte(text[start:end], '\n'); nl >= 0 {
				end = start + nl + 1
			}
		}
		chunks = append(chunks, text[start:end])
		start = end
	}
	return chunks
}

// chunkBudget returns the largest byte offset at or after start that stays
// within the code-unit budget, cutting before the rune that would overflow
// it. start is always a rune boundary.
func chunkBudget(text string, start int) int {
	units := 0
	for i := start; i < len(text); {
		r, size := utf8.DecodeRuneInString(text[i:])
		if units+utf16Units(r) > maxMessageUTF16CodeUnits {
			return i
		}
		units += utf16Units(r)
		i += size
	}
	return len(text)
}
