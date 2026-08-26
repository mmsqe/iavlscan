// Tree keys are opaque bytes to IAVL, but each store writes a module's own
// layout under them: a prefix naming the record, then its fields. Reading one
// back turns a damaged branch from 30 bytes of hex into "bank balance of
// mantra1...".
//
// The tables come from cosmos-sdk v0.53 and cosmos/evm, and are an aid rather
// than an authority: whatever they cannot place stays hex instead of a guess.
package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"strings"

	"github.com/cosmos/btcutil/bech32"
)

// bech32HRP is the chain's account prefix, from -bech32. Empty renders
// addresses as hex.
var bech32HRP = "mantra"

// addrKind picks an address's bech32 suffix; the sdk derives all three from
// the one account prefix.
type addrKind int

const (
	accAddr addrKind = iota
	valAddr
	consAddr
)

func (k addrKind) hrp() string {
	switch {
	case bech32HRP == "":
		return ""
	case k == valAddr:
		return bech32HRP + "valoper"
	case k == consAddr:
		return bech32HRP + "valcons"
	}
	return bech32HRP
}

// A render turns one field's bytes into text.
type render func([]byte) string

func hexs(b []byte) string   { return fmt.Sprintf("%x", b) }
func hex0x(b []byte) string  { return fmt.Sprintf("0x%x", b) }
func quoted(b []byte) string { return fmt.Sprintf("%q", b) }

// addr bech32-encodes an address, or falls back to hex when it cannot.
func addr(kind addrKind) render {
	return func(b []byte) string {
		hrp := kind.hrp()
		if hrp == "" {
			return hexs(b)
		}
		if s, err := bech32.EncodeFromBase256(hrp, b); err == nil {
			return s
		}
		return hexs(b)
	}
}

// inverted undoes the bitwise complement the validator power index stores so
// that higher power sorts first.
func inverted(r render) render {
	return func(b []byte) string {
		inv := make([]byte, len(b))
		for i, c := range b {
			inv[i] = ^c
		}
		return r(inv)
	}
}

// A reader consumes the front of a key. ok=false abandons the walk, leaving
// the rest to be printed as hex.
type reader func([]byte) (val string, rest []byte, ok bool)

// prefixed reads a one-byte length then that many bytes: address.MustLengthPrefix
// in the sdk's hand-rolled keys, and what collections writes for a non-terminal
// bytes key.
func prefixed(r render) reader {
	return func(b []byte) (string, []byte, bool) {
		if len(b) == 0 || len(b) < 1+int(b[0]) {
			return "", b, false
		}
		n := 1 + int(b[0])
		return r(b[1:n]), b[n:], true
	}
}

// tail reads everything left: a terminal collections key carries no length.
func tail(r render) reader {
	return func(b []byte) (string, []byte, bool) {
		if len(b) == 0 {
			return "", b, false
		}
		return r(b), nil, true
	}
}

// fixed reads exactly n bytes, for hashes, EVM addresses and slots.
func fixed(n int, r render) reader {
	return func(b []byte) (string, []byte, bool) {
		if len(b) < n {
			return "", b, false
		}
		return r(b[:n]), b[n:], true
	}
}

// strNull reads a non-terminal collections string key, which ends at a zero
// byte so ordering survives concatenation.
func strNull(b []byte) (string, []byte, bool) {
	i := bytes.IndexByte(b, 0)
	if i < 0 {
		return "", b, false
	}
	return quoted(b[:i]), b[i+1:], true
}

// u64 reads eight bytes. The sdk mixes orders even within one module, so each
// field names its own.
func u64(order binary.ByteOrder) reader {
	return func(b []byte) (string, []byte, bool) {
		if len(b) < 8 {
			return "", b, false
		}
		return fmt.Sprint(order.Uint64(b[:8])), b[8:], true
	}
}

// The readers the tables are written in.
var (
	be, le  = binary.BigEndian, binary.LittleEndian
	strRest = tail(quoted)
	evmAddr = fixed(20, hex0x)
	hash32  = fixed(32, hexs)
)

func lenAddr(k addrKind) reader    { return prefixed(addr(k)) }
func invLenAddr(k addrKind) reader { return prefixed(inverted(addr(k))) }
func restAddr(k addrKind) reader   { return tail(addr(k)) }

// field is one named component of a key.
type field struct {
	name string
	read reader
}

// layout is one record type: the prefix naming it and the fields that follow.
type layout struct {
	prefix []byte
	name   string
	fields []field
}

func rec(prefix byte, name string, fields ...field) layout {
	return layout{prefix: []byte{prefix}, name: name, fields: fields}
}

// keyLayouts maps a store name to its record types. Prefixes are matched
// longest first, so auth's "accountNumber" wins over any single byte.
var keyLayouts = map[string][]layout{
	// x/auth. Accounts keys on a terminal AccAddressKey, so the address is raw;
	// the by-number index prefixes with the literal string.
	"acc": {
		rec(0x00, "Params"),
		rec(0x01, "Account", field{"addr", restAddr(accAddr)}),
		rec(0x02, "GlobalAccountNumber"),
		rec(0x5a, "UnorderedNonces"),
		{prefix: []byte("accountNumber"), name: "AccountByNumber",
			fields: []field{{"number", u64(be)}}},
	},
	// x/bank. Balances is (address, denom); the denom index reverses the pair,
	// so there the denom leads and is zero-terminated while the address keeps
	// its length prefix.
	"bank": {
		rec(0x00, "Supply", field{"denom", strRest}),
		rec(0x01, "DenomMetadata", field{"denom", strRest}),
		rec(0x02, "Balance", field{"holder", lenAddr(accAddr)}, field{"denom", strRest}),
		rec(0x03, "DenomAddress", field{"denom", strNull}, field{"holder", lenAddr(accAddr)}),
		rec(0x04, "SendEnabled", field{"denom", strRest}),
		rec(0x05, "Params"),
	},
	"staking": {
		rec(0x11, "LastValidatorPower", field{"val", lenAddr(valAddr)}),
		rec(0x12, "LastTotalPower"),
		rec(0x21, "Validator", field{"val", lenAddr(valAddr)}),
		rec(0x22, "ValidatorByConsAddr", field{"cons", lenAddr(consAddr)}),
		rec(0x23, "ValidatorByPowerIndex", field{"power", u64(be)}, field{"val", invLenAddr(valAddr)}),
		rec(0x31, "Delegation", field{"del", lenAddr(accAddr)}, field{"val", lenAddr(valAddr)}),
		rec(0x32, "UnbondingDelegation", field{"del", lenAddr(accAddr)}, field{"val", lenAddr(valAddr)}),
		rec(0x33, "UnbondingDelegationByValIndex", field{"val", lenAddr(valAddr)}, field{"del", lenAddr(accAddr)}),
		rec(0x34, "Redelegation", field{"del", lenAddr(accAddr)}, field{"src", lenAddr(valAddr)}, field{"dst", lenAddr(valAddr)}),
		rec(0x35, "RedelegationByValSrc", field{"src", lenAddr(valAddr)}, field{"del", lenAddr(accAddr)}, field{"dst", lenAddr(valAddr)}),
		rec(0x36, "RedelegationByValDst", field{"dst", lenAddr(valAddr)}, field{"del", lenAddr(accAddr)}, field{"src", lenAddr(valAddr)}),
		rec(0x37, "UnbondingID"),
		rec(0x38, "UnbondingIndex", field{"id", u64(be)}),
		rec(0x39, "UnbondingType", field{"id", u64(be)}),
		rec(0x41, "UnbondingQueue"),
		rec(0x42, "RedelegationQueue"),
		rec(0x43, "ValidatorQueue"),
		rec(0x50, "HistoricalInfo", field{"height", u64(be)}),
		rec(0x51, "Params"),
		rec(0x61, "ValidatorUpdates"),
		// GetDelegationsByValKey appends the delegator raw, without a length.
		rec(0x71, "DelegationByValIndex", field{"val", lenAddr(valAddr)}, field{"del", restAddr(accAddr)}),
	},
	// x/distribution. Historical rewards store the period little-endian, slash
	// events store height and period big-endian.
	"distribution": {
		rec(0x00, "FeePool"),
		rec(0x01, "Proposer"),
		rec(0x02, "ValidatorOutstandingRewards", field{"val", lenAddr(valAddr)}),
		rec(0x03, "DelegatorWithdrawAddr", field{"del", lenAddr(accAddr)}),
		rec(0x04, "DelegatorStartingInfo", field{"val", lenAddr(valAddr)}, field{"del", lenAddr(accAddr)}),
		rec(0x05, "ValidatorHistoricalRewards", field{"val", lenAddr(valAddr)}, field{"period", u64(le)}),
		rec(0x06, "ValidatorCurrentRewards", field{"val", lenAddr(valAddr)}),
		rec(0x07, "ValidatorAccumulatedCommission", field{"val", lenAddr(valAddr)}),
		rec(0x08, "ValidatorSlashEvent", field{"val", lenAddr(valAddr)}, field{"height", u64(be)}, field{"period", u64(be)}),
		rec(0x09, "Params"),
	},
	// x/slashing. The missed-block bitmap is chunked, and its chunk index is the
	// module's one little-endian integer.
	"slashing": {
		rec(0x00, "Params"),
		rec(0x01, "ValidatorSigningInfo", field{"cons", lenAddr(consAddr)}),
		rec(0x02, "ValidatorMissedBlockBitmap", field{"cons", lenAddr(consAddr)}, field{"chunk", u64(le)}),
		rec(0x03, "AddrPubkeyRelation", field{"cons", lenAddr(consAddr)}),
	},
	"mint": {
		rec(0x00, "Minter"),
		rec(0x01, "Params"),
	},
	"gov": {
		rec(0x00, "Proposal", field{"id", u64(be)}),
		rec(0x01, "ActiveProposalQueue"),
		rec(0x02, "InactiveProposalQueue"),
		rec(0x03, "ProposalID"),
		rec(0x04, "VotingPeriodProposal", field{"id", u64(be)}),
		rec(0x10, "Deposit", field{"id", u64(be)}, field{"depositor", lenAddr(accAddr)}),
		rec(0x20, "Vote", field{"id", u64(be)}, field{"voter", lenAddr(accAddr)}),
		rec(0x30, "Params"),
		rec(0x31, "Constitution"),
	},
	// x/vm. Code is keyed by the code hash, storage by contract address then
	// slot, both raw and unprefixed.
	"evm": {
		rec(0x01, "Code", field{"codehash", hash32}),
		rec(0x02, "Storage", field{"contract", evmAddr}, field{"slot", hash32}),
		rec(0x03, "Params"),
		rec(0x04, "CodeHash", field{"contract", evmAddr}),
		rec(0x05, "EvmCoinInfo"),
	},
	"erc20": {
		rec(0x01, "TokenPair", field{"id", hash32}),
		rec(0x02, "TokenPairByERC20", field{"erc20", evmAddr}),
		rec(0x03, "TokenPairByDenom", field{"denom", strRest}),
		rec(0x04, "STRv2Address"),
		rec(0x05, "Allowance"),
		rec(0x06, "NativePrecompile"),
		rec(0x07, "DynamicPrecompile"),
	},
	"feemarket": {
		rec(0x01, "BlockGasWanted"),
	},
}

// matchLayout returns the longest prefix in the store's table that the key
// starts with, or false when the store or the prefix is unknown.
func matchLayout(store string, key []byte) (layout, bool) {
	var best layout
	for _, l := range keyLayouts[store] {
		if bytes.HasPrefix(key, l.prefix) && len(l.prefix) > len(best.prefix) {
			best = l
		}
	}
	return best, best.prefix != nil
}

// render walks the fields over the key's body. Whatever the layout cannot
// place becomes a rest= tail rather than being dropped: the record is still
// named, and the tail is the evidence that the layout is wrong for this key.
func (l layout) render(store string, key []byte) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s/%s", store, l.name)
	rest := key[len(l.prefix):]
	for _, f := range l.fields {
		val, next, ok := f.read(rest)
		if !ok {
			break
		}
		fmt.Fprintf(&b, " %s=%s", f.name, val)
		rest = next
	}
	if len(rest) > 0 {
		fmt.Fprintf(&b, " rest=%x", rest)
	}
	return b.String()
}

// describeKey is describe for a tree key: hex first, so it still pastes into
// `pebble find`, then the module record when the prefix is known.
func describeKey(store string, key []byte) string {
	out := describe(key)
	if l, ok := matchLayout(store, key); ok {
		out += " (" + l.render(store, key) + ")"
	}
	return out
}
