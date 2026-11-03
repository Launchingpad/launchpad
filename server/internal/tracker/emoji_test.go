package tracker

import (
	"errors"
	"testing"

	"devops/internal/httpx"
)

func TestNormalizeProjectEmoji(t *testing.T) {
	ok := map[string]string{
		"plain":             "\U0001F680",            // rocket
		"trimmed":           "  \U0001F389 ",         // party popper
		"text style sign":   "❤",                     // heart
		"emoji style":       "❤️",                    // heart VS16
		"bmp symbol":        "⭐",                     // star
		"copyright vs16":    "©️",                    // ©️
		"registered":        "®",                     // ®
		"trade mark":        "™️",                    // ™️
		"double excl":       "‼️",                    // ‼️
		"info":              "ℹ️",                    // ℹ️
		"left right arrow":  "↔️",                    // ↔️
		"return arrow":      "↩️",                    // ↩️
		"watch":             "⌚",                     // ⌚
		"white med square":  "◻️",                    // ◻️
		"black med square":  "◼️",                    // ◼️
		"white small sq":    "◽",                     // ◽
		"black small sq":    "◾",                     // ◾
		"arrow up curve":    "⤴️",                    // ⤴️
		"arrow down curve":  "⤵️",                    // ⤵️
		"left arrow":        "⬅️",                    // ⬅️
		"large square":      "⬛",                     // ⬛
		"circle":            "⭕",                     // ⭕
		"wavy dash":         "〰️",                    // 〰️
		"part alternation":  "〽️",                    // 〽️
		"circled secret":    "㊙️",                    // ㊙️
		"circled m":         "Ⓜ️",                    // Ⓜ️
		"skin tone":         "\U0001F44D\U0001F3FD",  // 👍🏽
		"skin tone bmp":     "✋\U0001F3FF",           // ✋🏿
		"zwj profession":    "\U0001F469‍\U0001F4BB", // 👩‍💻
		"zwj skin profess":  "\U0001F9D1\U0001F3FD‍\U0001F4BB",
		"zwj rainbow flag":  "\U0001F3F3️‍\U0001F308",
		"zwj vs16 middle":   "❤️‍\U0001F525",         // ❤️‍🔥
		"zwj gender sign":   "\U0001F3C3‍♀️",         // 🏃‍♀️
		"zwj hair":          "\U0001F468‍\U0001F9B0", // 👨‍🦰
		"handshake tones":   "\U0001F9D1\U0001F3FD‍\U0001F91D‍\U0001F9D1\U0001F3FF",
		"flag":              "\U0001F1EC\U0001F1E7", // 🇬🇧
		"black flag alone":  "\U0001F3F4",           // 🏴
		"keycap":            "1️⃣",                  // 1️⃣
		"keycap bare":       "#⃣",                   // #⃣
		"keycap star":       "*️⃣",                  // *️⃣
		"family of three":   "\U0001F468‍\U0001F469‍\U0001F467",
		"family of four":    "\U0001F468‍\U0001F469‍\U0001F467‍\U0001F466",
		"couple skin tones": "\U0001F469\U0001F3FD‍❤️‍\U0001F468\U0001F3FF",
		"kiss one tone":     "\U0001F469‍❤️‍\U0001F48B‍\U0001F468\U0001F3FF",
		"england":           "\U0001F3F4\U000E0067\U000E0062\U000E0065\U000E006E\U000E0067\U000E007F",
		"scotland":          "\U0001F3F4\U000E0067\U000E0062\U000E0073\U000E0063\U000E0074\U000E007F",
		"wales":             "\U0001F3F4\U000E0067\U000E0062\U000E0077\U000E006C\U000E0073\U000E007F",
		// The check is structural, so a 32-byte ZWJ chain of pictographs passes (the limit is inclusive).
		"32 bytes": "\U0001F468\U0001F3FD\u200D\u2764\uFE0F\u200D\u2764\uFE0F\u200D\u2764",
	}
	for name, in := range ok {
		t.Run("ok/"+name, func(t *testing.T) {
			got, err := normalizeProjectEmoji(in)
			if err != nil {
				t.Fatalf("%q: %v", in, err)
			}
			if got == nil || len(*got) > maxEmojiBytes {
				t.Fatalf("%q: got %v", in, got)
			}
		})
	}
	for _, in := range []string{"", "   "} {
		got, err := normalizeProjectEmoji(in)
		if err != nil || got != nil {
			t.Fatalf("%q must clear: %v %v", in, got, err)
		}
	}
	bad := map[string]string{
		"letter":             "a",
		"word":               "rocket",
		"digit":              "7",
		"text and emoji":     "\U0001F680a",
		"two emoji":          "\U0001F680\U0001F389",
		"three flags ri":     "\U0001F1EC\U0001F1E7\U0001F1FA",
		"two flags":          "\U0001F1EC\U0001F1E7\U0001F1FA\U0001F1F8",
		"lone ri":            "\U0001F1EC",
		"lone skin tone":     "\U0001F3FD",
		"lone zwj":           "‍",
		"trailing zwj":       "\U0001F469‍",
		"leading zwj":        "‍\U0001F469",
		"double zwj":         "\U0001F469‍‍\U0001F4BB",
		"control nul":        "\U0001F680\x00",
		"control bell":       "\a",
		"control in zwj":     "\U0001F469‍\x7f",
		"newline inside":     "\U0001F680\n\U0001F389",
		"invalid utf8":       "\xff",
		"33 bytes":           "❤️‍❤️‍❤️‍❤️",
		"kiss two tones":     "\U0001F469\U0001F3FD‍❤️‍\U0001F48B‍\U0001F468\U0001F3FF",
		"many and text":      "\U0001F680\U0001F680\U0001F680\U0001F680a",
		"keycap no mark":     "1️",
		"combining accent":   "\U0001F680́",
		"private use":        "",
		"space inside":       "\U0001F680 \U0001F389",
		"place of interest":  "⌘",                     // ⌘ is not an emoji
		"full block":         "█",                     // █
		"braille blank":      "⠀",                     // looks empty
		"replacement char":   "�",                     // �
		"object replacement": "￼",                     // ￼
		"rial sign":          "﷽",                     // a wide Arabic ligature
		"circled letter":     "ⓐ",                     // ⓐ
		"circled zwj chain":  "ⓐ‍ⓑ",                   // ⓐ‍ⓑ
		"squared letter":     "\U0001F130",            // 🄰
		"squared zwj chain":  "\U0001F130‍\U0001F131", // 🄰‍🄱
		"letter zwj emoji":   "a‍\U0001F680",          // a‍🚀
		"skin after rocket":  "\U0001F680\U0001F3FD",  // a base that takes no skin tone
		"skin after heart":   "❤\U0001F3FD",           //
		"tag no cancel":      "\U0001F3F4\U000E0067",  //
		"tag one letter":     "\U0001F3F4\U000E0067\U000E007F",
		"tag hidden ascii":   "\U0001F3F4\U000E0068\U000E0069\U000E007F", // "hi"
		"tag on rocket":      "\U0001F680\U000E0067\U000E0062\U000E0065\U000E006E\U000E0067\U000E007F",
		"tag us state":       "\U0001F3F4\U000E0075\U000E0073\U000E0074\U000E0078\U000E007F", // ustx: not RGI
		"32 bytes tag":       "\U0001F3F4\U000E0067\U000E0062\U000E0065\U000E006E\U000E0067\U000E0067\U000E007F",
	}
	// The size check comes first: 33 bytes is refused for its size.
	if _, err := normalizeProjectEmoji(bad["33 bytes"]); err == nil || err.Error() != "emoji must be at most 32 bytes" {
		t.Fatalf("33 bytes: %v", err)
	}
	if n := len(ok["32 bytes"]); n != 32 {
		t.Fatalf("the 32-byte case is %d bytes", n)
	}
	for name, in := range bad {
		t.Run("bad/"+name, func(t *testing.T) {
			got, err := normalizeProjectEmoji(in)
			var he *httpx.Error
			if err == nil || !errors.As(err, &he) || he.Code != httpx.CodeValidationFailed {
				t.Fatalf("%q: want validation_failed, got %v %v", in, got, err)
			}
		})
	}
}

// The range tables are sorted and do not overlap (inRanges needs it), and
// every modifier base is a pictograph.
func TestEmojiTables(t *testing.T) {
	for name, tab := range map[string][]runeRange{"pictographic": extendedPictographic, "modifierBase": emojiModifierBase} {
		for i, r := range tab {
			if r.lo > r.hi || (i > 0 && tab[i-1].hi >= r.lo) {
				t.Fatalf("%s: range %d (%X-%X) is out of order", name, i, r.lo, r.hi)
			}
		}
	}
	for _, r := range emojiModifierBase {
		for c := r.lo; c <= r.hi; c++ {
			if !isEmojiBase(c) {
				t.Fatalf("modifier base %X is not pictographic", c)
			}
		}
	}
	for _, c := range []rune{skinFirst, skinLast, riFirst, riLast, zwj, vs16, 'A', '0'} {
		if isEmojiBase(c) {
			t.Fatalf("%X must not be a base", c)
		}
	}
}
