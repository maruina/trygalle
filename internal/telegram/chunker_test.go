package telegram

import (
	"strings"
	"testing"
)

// utf16UnitsOf reports the UTF-16 code units a byte slice of runes costs,
// mirroring the chunker's own accounting.
func utf16UnitsOf(_ *testing.T, s string) int {
	units := 0
	for _, r := range s {
		if r >= 0x10000 {
			units += 2
		} else {
			units++
		}
	}
	return units
}

func TestChunk(t *testing.T) {
	t.Parallel()

	ascii := func(n int) string { return strings.Repeat("a", n) }
	emoji := func(n int) string { return strings.Repeat("\U0001F600", n) }

	tests := []struct {
		name       string
		give       string
		wantChunks int
		wantFirst  string
		check      func(t *testing.T, got []string)
	}{
		{
			name:       "exactly 4096 units stays one message",
			give:       ascii(maxMessageUTF16CodeUnits),
			wantChunks: 1,
			wantFirst:  ascii(maxMessageUTF16CodeUnits),
		},
		{
			name:       "one unit over splits into two",
			give:       ascii(maxMessageUTF16CodeUnits + 1),
			wantChunks: 2,
			wantFirst:  ascii(maxMessageUTF16CodeUnits),
		},
		{
			name:       "multi-byte character at the boundary never splits",
			give:       ascii(maxMessageUTF16CodeUnits) + "éé", // 4096 + 2 units
			wantChunks: 2,
			wantFirst:  ascii(maxMessageUTF16CodeUnits),
		},
		{
			name:       "astral-plane runes cost two units and chunk",
			give:       emoji(maxMessageUTF16CodeUnits), // 4096 runes = 8192 units
			wantChunks: 2,
			wantFirst:  emoji(maxMessageUTF16CodeUnits / 2),
		},
		{
			name:       "astral-plane runes within the unit budget stay one message",
			give:       emoji(maxMessageUTF16CodeUnits / 2),
			wantChunks: 1,
		},
		{
			name:       "line break preferred over mid-line split",
			give:       ascii(2000) + "\n" + ascii(3000) + "\n" + ascii(100),
			wantChunks: 2,
			wantFirst:  ascii(2000) + "\n",
			check: func(t *testing.T, got []string) {
				t.Helper()
				if want := ascii(3000) + "\n" + ascii(100); got[1] != want {
					t.Errorf("want second chunk to start after the line break")
				}
			},
		},
		{
			name:       "mid-line hard split only on a too-long line",
			give:       ascii(maxMessageUTF16CodeUnits + 500),
			wantChunks: 2,
		},
		{
			name: "no empty trailing chunk from trailing lines",
			give: ascii(3000) + "\n\n\n" + ascii(3000) + "\n\n",
			check: func(t *testing.T, got []string) {
				t.Helper()
				for i, c := range got {
					if unix16 := utf16UnitsOf(t, c); unix16 > maxMessageUTF16CodeUnits {
						t.Errorf("chunk %d has %d units", i, unix16)
					}
				}
				for i, c := range got[:len(got)-1] {
					if len(c) == 0 {
						t.Errorf("chunk %d is empty", i)
					}
				}
			},
		},
		{
			name:       "long Pi response chunk count",
			give:       ascii(maxMessageUTF16CodeUnits*2 + 17),
			wantChunks: 3,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := chunk(tt.give)
			if tt.wantChunks != 0 && len(got) != tt.wantChunks {
				t.Fatalf("chunk() returned %d chunks, want %d", len(got), tt.wantChunks)
			}
			if tt.wantFirst != "" && got[0] != tt.wantFirst {
				t.Errorf("chunk()[0] = %d chars, want %d exact match", len(got[0]), len(tt.wantFirst))
			}
			joining := strings.Join(got, "")
			if joining != tt.give {
				t.Errorf("chunks lost or corrupted input: joined len %d, input len %d",
					len(joining), len(tt.give))
			}
			for i, c := range got {
				if units := utf16UnitsOf(t, c); units > maxMessageUTF16CodeUnits {
					t.Errorf("chunk %d has %d UTF-16 units, over the limit", i, units)
				}
			}
			if tt.check != nil {
				tt.check(t, got)
			}
		})
	}
}

func TestChunkWithinLimitUnchanged(t *testing.T) {
	t.Parallel()
	short := "hello `world` and \U0001F600"
	got := chunk(short)
	if len(got) != 1 || got[0] != short {
		t.Errorf("chunk(%q) = %q, want single unchanged chunk", short, got)
	}
}

func TestChunkRoundTripAtRuneBoundaries(t *testing.T) {
	t.Parallel()
	// Multi-byte characters densely around every boundary.
	var parts []string
	for range 3 {
		parts = append(parts, strings.Repeat("aé\U0001F600b", 1000))
	}
	give := strings.Join(parts, "\nline\n")
	got := chunk(give)
	if joining := strings.Join(got, ""); joining != give {
		t.Errorf("chunk round trip lost content")
	}
	for _, c := range got {
		if c == "" {
			continue
		}
		if c[len(c)-1] == 0xEF || (c[len(c)-1]&0b1100_0000) == 0b1000_0000 {
			t.Errorf("chunk ends inside a UTF-8 sequence: %q", c[len(c)-min(3, len(c)):])
		}
	}
}
