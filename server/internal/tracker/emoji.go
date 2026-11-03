package tracker

import (
	"slices"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"devops/internal/httpx"
)

// maxEmojiBytes is the size limit of a project emoji (decisions-T41 #2). The
// projects_emoji_check constraint holds the same number.
const maxEmojiBytes = 32

const (
	zwj         = '‍' // ZERO WIDTH JOINER
	vs15        = '︎' // VARIATION SELECTOR-15 (text style)
	vs16        = '️' // VARIATION SELECTOR-16 (emoji style)
	keycapMark  = '⃣' // COMBINING ENCLOSING KEYCAP
	blackFlag   = '\U0001F3F4'
	tagCancel   = '\U000E007F'
	riFirst     = '\U0001F1E6' // REGIONAL INDICATOR SYMBOL LETTER A
	riLast      = '\U0001F1FF'
	skinFirst   = '\U0001F3FB' // EMOJI MODIFIER FITZPATRICK TYPE-1-2
	skinLast    = '\U0001F3FF'
	keycapBases = "0123456789#*"
)

// runeRange is a closed range of code points.
type runeRange struct{ lo, hi rune }

// inRanges reports whether r is in rs (sorted, not overlapping).
func inRanges(rs []runeRange, r rune) bool {
	i := sort.Search(len(rs), func(i int) bool { return rs[i].hi >= r })
	return i < len(rs) && rs[i].lo <= r
}

// extendedPictographic is the Extended_Pictographic property of Unicode
// (emoji-data.txt, Unicode 15.1). The Go standard library has no table of it.
// It holds every emoji pictograph (and the code points Unicode keeps for future
// ones), and no letter, digit, block element, braille or replacement character.
var extendedPictographic = []runeRange{
	{0x00A9, 0x00A9}, {0x00AE, 0x00AE}, {0x203C, 0x203C}, {0x2049, 0x2049},
	{0x2122, 0x2122}, {0x2139, 0x2139}, {0x2194, 0x2199}, {0x21A9, 0x21AA},
	{0x231A, 0x231B}, {0x2328, 0x2328}, {0x2388, 0x2388}, {0x23CF, 0x23CF},
	{0x23E9, 0x23F3}, {0x23F8, 0x23FA}, {0x24C2, 0x24C2}, {0x25AA, 0x25AB},
	{0x25B6, 0x25B6}, {0x25C0, 0x25C0}, {0x25FB, 0x25FE}, {0x2600, 0x2605},
	{0x2607, 0x2612}, {0x2614, 0x2685}, {0x2690, 0x2705}, {0x2708, 0x2712},
	{0x2714, 0x2714}, {0x2716, 0x2716}, {0x271D, 0x271D}, {0x2721, 0x2721},
	{0x2728, 0x2728}, {0x2733, 0x2734}, {0x2744, 0x2744}, {0x2747, 0x2747},
	{0x274C, 0x274C}, {0x274E, 0x274E}, {0x2753, 0x2755}, {0x2757, 0x2757},
	{0x2763, 0x2767}, {0x2795, 0x2797}, {0x27A1, 0x27A1}, {0x27B0, 0x27B0},
	{0x27BF, 0x27BF}, {0x2934, 0x2935}, {0x2B05, 0x2B07}, {0x2B1B, 0x2B1C},
	{0x2B50, 0x2B50}, {0x2B55, 0x2B55}, {0x3030, 0x3030}, {0x303D, 0x303D},
	{0x3297, 0x3297}, {0x3299, 0x3299},
	{0x1F000, 0x1F0FF}, {0x1F10D, 0x1F10F}, {0x1F12F, 0x1F12F}, {0x1F16C, 0x1F171},
	{0x1F17E, 0x1F17F}, {0x1F18E, 0x1F18E}, {0x1F191, 0x1F19A}, {0x1F1AD, 0x1F1E5},
	{0x1F201, 0x1F20F}, {0x1F21A, 0x1F21A}, {0x1F22F, 0x1F22F}, {0x1F232, 0x1F23A},
	{0x1F23C, 0x1F23F}, {0x1F249, 0x1F3FA}, {0x1F400, 0x1F53D}, {0x1F546, 0x1F64F},
	{0x1F680, 0x1F6FF}, {0x1F774, 0x1F77F}, {0x1F7D5, 0x1F7FF}, {0x1F80C, 0x1F80F},
	{0x1F848, 0x1F84F}, {0x1F85A, 0x1F85F}, {0x1F888, 0x1F88F}, {0x1F8AE, 0x1F8FF},
	{0x1F90C, 0x1F93A}, {0x1F93C, 0x1F945}, {0x1F947, 0x1FAFF}, {0x1FC00, 0x1FFFD},
}

// emojiModifierBase is the Emoji_Modifier_Base property (emoji-data.txt,
// Unicode 15.1): the only emoji a skin tone modifier may follow.
var emojiModifierBase = []runeRange{
	{0x261D, 0x261D}, {0x26F9, 0x26F9}, {0x270A, 0x270D},
	{0x1F385, 0x1F385}, {0x1F3C2, 0x1F3C4}, {0x1F3C7, 0x1F3C7}, {0x1F3CA, 0x1F3CC},
	{0x1F442, 0x1F443}, {0x1F446, 0x1F450}, {0x1F466, 0x1F478}, {0x1F47C, 0x1F47C},
	{0x1F481, 0x1F483}, {0x1F485, 0x1F487}, {0x1F48F, 0x1F48F}, {0x1F491, 0x1F491},
	{0x1F4AA, 0x1F4AA}, {0x1F574, 0x1F575}, {0x1F57A, 0x1F57A}, {0x1F590, 0x1F590},
	{0x1F595, 0x1F596}, {0x1F645, 0x1F647}, {0x1F64B, 0x1F64F}, {0x1F6A3, 0x1F6A3},
	{0x1F6B4, 0x1F6B6}, {0x1F6C0, 0x1F6C0}, {0x1F6CC, 0x1F6CC}, {0x1F90C, 0x1F90C},
	{0x1F90F, 0x1F90F}, {0x1F918, 0x1F91F}, {0x1F926, 0x1F926}, {0x1F930, 0x1F939},
	{0x1F93C, 0x1F93E}, {0x1F977, 0x1F977}, {0x1F9B5, 0x1F9B6}, {0x1F9B8, 0x1F9B9},
	{0x1F9BB, 0x1F9BB}, {0x1F9CD, 0x1F9CF}, {0x1F9D1, 0x1F9DD}, {0x1FAC3, 0x1FAC5},
	{0x1FAF0, 0x1FAF8},
}

// subdivisionFlags are the only tag sequences accepted: the RGI flags of
// England, Scotland and Wales (U+1F3F4, the tag letters, CANCEL TAG). Any
// other tag text is hidden ASCII and is refused.
var subdivisionFlags = []string{
	tagFlag("gbeng"), tagFlag("gbsct"), tagFlag("gbwls"),
}

func tagFlag(region string) string {
	var b strings.Builder
	b.WriteRune(blackFlag)
	for _, c := range region {
		b.WriteRune(0xE0000 + c)
	}
	b.WriteRune(tagCancel)
	return b.String()
}

func isRegional(r rune) bool { return r >= riFirst && r <= riLast }
func isSkinTone(r rune) bool { return r >= skinFirst && r <= skinLast }

// isEmojiBase is a rune that can start an emoji element: an
// Extended_Pictographic code point.
func isEmojiBase(r rune) bool { return inRanges(extendedPictographic, r) }

// isOneEmoji reports whether rs is exactly one emoji sequence (UTS #51):
// a flag (two regional indicators), a keycap, one of the three subdivision
// flags, or one or more emoji elements joined by ZERO WIDTH JOINER. An element
// is a pictograph, an optional variation selector and, after a modifier base
// only, an optional skin tone. Each of these is one grapheme cluster (UAX #29).
func isOneEmoji(rs []rune) bool {
	n := len(rs)
	if n == 0 {
		return false
	}
	// Flag: exactly two regional indicators.
	if isRegional(rs[0]) {
		return n == 2 && isRegional(rs[1])
	}
	// Keycap: [0-9#*] VS16? U+20E3.
	if strings.ContainsRune(keycapBases, rs[0]) {
		return (n == 2 && rs[1] == keycapMark) || (n == 3 && rs[1] == vs16 && rs[2] == keycapMark)
	}
	// Tag sequence: one of the three subdivision flags, nothing else.
	if n > 1 && rs[1] >= 0xE0000 && rs[1] <= tagCancel {
		return slices.Contains(subdivisionFlags, string(rs))
	}
	// ZWJ sequence of elements (one element is the common case).
	i := 0
	for {
		if i >= n || !isEmojiBase(rs[i]) {
			return false
		}
		base := rs[i]
		i++
		if i < n && (rs[i] == vs16 || rs[i] == vs15) {
			i++
		}
		if i < n && isSkinTone(rs[i]) {
			if !inRanges(emojiModifierBase, base) {
				return false
			}
			i++
		}
		if i == n {
			return true
		}
		if rs[i] != zwj {
			return false
		}
		i++
	}
}

// normalizeProjectEmoji checks a project emoji (decisions-T41). Surrounding
// white space is dropped; an empty value means none (nil). Anything else must
// be valid UTF-8, at most maxEmojiBytes bytes, hold no control character and
// be exactly one emoji sequence.
func normalizeProjectEmoji(v string) (*string, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil, nil
	}
	if !utf8.ValidString(v) {
		return nil, httpx.Invalid("emoji must be valid UTF-8")
	}
	if len(v) > maxEmojiBytes {
		return nil, httpx.Invalid("emoji must be at most %d bytes", maxEmojiBytes)
	}
	rs := []rune(v)
	for _, r := range rs {
		if unicode.IsControl(r) {
			return nil, httpx.Invalid("emoji must not hold a control character")
		}
	}
	if !isOneEmoji(rs) {
		return nil, httpx.Invalid("emoji must be exactly one emoji")
	}
	return &v, nil
}
