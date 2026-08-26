package main

import (
	"encoding/hex"
	"strings"
	"testing"
)

// mustHex is the key bytes as `pebble find` prints them.
func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("decode %q: %v", s, err)
	}
	return b
}

// TestDescribeKey covers one record per layout shape: the length-prefixed
// address, the zero-terminated string, the unterminated tail string, the raw
// terminal address, both integer orders, and the fixed-width EVM fields.
func TestDescribeKey(t *testing.T) {
	const (
		acc  = "a6fde35439d38ac58780da940371ed87993ae6e2"
		acc2 = "0102030405060708090a0b0c0d0e0f1011121314"
		// Same 20 bytes, three prefixes: the checksum is over the hrp too, so
		// the tail differs even though the data part does not.
		bech32Acc     = "mantra15m77x4pe6w9vtpuqm22qxu0ds7vn4ehzwx8pls"
		bech32Valoper = "mantravaloper1qypqxpq9qcrsszg2pvxq6rs0zqg3yyc5sj85fr"
		bech32Valcons = "mantravalcons1qypqxpq9qcrsszg2pvxq6rs0zqg3yyc5yp5g9z"
	)
	for _, tc := range []struct {
		name  string
		store string
		key   string
		want  string
	}{{
		// The reverse index that turned up in the fixture DB: denom leads and
		// is zero-terminated, the address keeps its length prefix.
		name:  "bank denom address index",
		store: "bank",
		key:   "03" + "616d616e747261" + "00" + "14" + acc,
		want:  `bank/DenomAddress denom="amantra" holder=` + bech32Acc,
	}, {
		// The primary balance key is the same pair the other way round, so the
		// denom is terminal and carries no zero byte.
		name:  "bank balance",
		store: "bank",
		key:   "02" + "14" + acc + "616d616e747261",
		want:  "bank/Balance holder=" + bech32Acc + ` denom="amantra"`,
	}, {
		name:  "bank supply",
		store: "bank",
		key:   "00" + "616d616e747261",
		want:  `bank/Supply denom="amantra"`,
	}, {
		name:  "staking delegation",
		store: "staking",
		key:   "31" + "14" + acc + "14" + acc2,
		want:  "staking/Delegation del=" + bech32Acc + " val=" + bech32Valoper,
	}, {
		// Only the power index inverts the address, so that higher power sorts first.
		name:  "staking power index",
		store: "staking",
		key:   "23" + "0000000000000064" + "14" + "fefdfcfbfaf9f8f7f6f5f4f3f2f1f0efeeedeceb",
		want:  "staking/ValidatorByPowerIndex power=100 val=" + bech32Valoper,
	}, {
		name:  "distribution historical rewards period is little endian",
		store: "distribution",
		key:   "05" + "14" + acc2 + "0700000000000000",
		want:  "distribution/ValidatorHistoricalRewards val=" + bech32Valoper + " period=7",
	}, {
		name:  "distribution slash event is big endian",
		store: "distribution",
		key:   "08" + "14" + acc2 + "0000000000000009" + "000000000000000b",
		want:  "distribution/ValidatorSlashEvent val=" + bech32Valoper + " height=9 period=11",
	}, {
		name:  "slashing signing info uses the consensus prefix",
		store: "slashing",
		key:   "01" + "14" + acc2,
		want:  "slashing/ValidatorSigningInfo cons=" + bech32Valcons,
	}, {
		// ValidatorMissedBlockBitmapKey writes the chunk index little-endian.
		name:  "slashing missed block bitmap chunk",
		store: "slashing",
		key:   "02" + "14" + acc2 + "0300000000000000",
		want:  "slashing/ValidatorMissedBlockBitmap cons=" + bech32Valcons + " chunk=3",
	}, {
		// The height field the fixture DB turned up as a rest= tail before it
		// was added, which is what a missing field looks like.
		name:  "staking historical info",
		store: "staking",
		key:   "50" + "0000000000000003",
		want:  "staking/HistoricalInfo height=3",
	}, {
		// auth stores the account address raw, with no length prefix.
		name:  "auth account",
		store: "acc",
		key:   "01" + acc,
		want:  "acc/Account addr=" + bech32Acc,
	}, {
		// The multi-byte string prefix has to beat the single-byte ones.
		name:  "auth account by number",
		store: "acc",
		key:   hex.EncodeToString([]byte("accountNumber")) + "0000000000000005",
		want:  "acc/AccountByNumber number=5",
	}, {
		name:  "evm storage slot",
		store: "evm",
		key:   "02" + acc2 + strings.Repeat("ab", 32),
		want:  "evm/Storage contract=0x0102030405060708090a0b0c0d0e0f1011121314 slot=" + strings.Repeat("ab", 32),
	}, {
		name:  "gov vote",
		store: "gov",
		key:   "20" + "0000000000000002" + "14" + acc,
		want:  "gov/Vote id=2 voter=" + bech32Acc,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			got := describeKey(tc.store, mustHex(t, tc.key))
			if !strings.HasPrefix(got, tc.key) {
				t.Fatalf("describeKey dropped the raw hex:\n got %s\nwant prefix %s", got, tc.key)
			}
			if want := "(" + tc.want + ")"; !strings.HasSuffix(got, want) {
				t.Fatalf("describeKey(%q):\n got %s\nwant suffix %s", tc.store, got, want)
			}
		})
	}
}

// TestDescribeKeyFallsBack checks that nothing is invented: an unknown store or
// prefix reports hex only, and a layout that does not fit the key still names
// the record but hands back the bytes it could not place.
func TestDescribeKeyFallsBack(t *testing.T) {
	for _, tc := range []struct {
		name, store, key, want string
	}{{
		name:  "unknown store",
		store: "nosuchstore",
		key:   "03616d616e747261",
		want:  "",
	}, {
		name:  "unknown prefix",
		store: "bank",
		key:   "ff0102",
		want:  "",
	}, {
		name:  "truncated address is left unread",
		store: "bank",
		key:   "02" + "14" + "0102",
		want:  "(bank/Balance rest=140102)",
	}, {
		name:  "unterminated denom is left unread",
		store: "bank",
		key:   "03" + "616d616e747261",
		want:  "(bank/DenomAddress rest=616d616e747261)",
	}, {
		name:  "trailing bytes are reported not dropped",
		store: "slashing",
		key:   "00" + "deadbeef",
		want:  "(slashing/Params rest=deadbeef)",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			got := describeKey(tc.store, mustHex(t, tc.key))
			switch {
			case tc.want == "" && strings.Contains(got, "("):
				t.Fatalf("describeKey(%q, %s) invented a reading: %s", tc.store, tc.key, got)
			case tc.want != "" && !strings.HasSuffix(got, tc.want):
				t.Fatalf("describeKey(%q, %s):\n got %s\nwant suffix %s", tc.store, tc.key, got, tc.want)
			}
		})
	}
}

// TestDescribeKeyWithoutBech32 checks that -bech32 "" degrades to hex rather
// than to a wrong prefix, for a chain this table was not written against.
func TestDescribeKeyWithoutBech32(t *testing.T) {
	defer func(h string) { bech32HRP = h }(bech32HRP)
	bech32HRP = ""

	got := describeKey("bank", mustHex(t, "0214a6fde35439d38ac58780da940371ed87993ae6e2616d616e747261"))
	want := `(bank/Balance holder=a6fde35439d38ac58780da940371ed87993ae6e2 denom="amantra")`
	if !strings.HasSuffix(got, want) {
		t.Fatalf("describeKey with no hrp:\n got %s\nwant suffix %s", got, want)
	}
}
