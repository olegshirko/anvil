package specutil

import (
	"testing"

	"github.com/elastic/go-seccomp-bpf"
	specs "github.com/opencontainers/runtime-spec/specs-go"
)

// TestBpfCondition covers the OCI-operator to BPF-condition mapping. The
// masked operators are the interesting part: the containerd default profile
// allows clone only when its namespace-flag bits are all clear
// (SCMP_CMP_MASKED_EQ with valuetwo == 0), which the single-value condition
// set expresses as BitsNotSet.
func TestBpfCondition(t *testing.T) {
	const nsFlags = uint64(0x00040000 | 0x00020000 | 0x08000000 | 0x10000000 | 0x04000000 | 0x40000000 | 0x20000000)
	cases := []struct {
		name string
		arg  specs.LinuxSeccompArg
		op   seccomp.Operation
		val  uint64
		ok   bool
	}{
		{"equal", specs.LinuxSeccompArg{Index: 0, Value: 42, Op: specs.OpEqualTo}, seccomp.Equal, 42, true},
		{"not equal", specs.LinuxSeccompArg{Index: 1, Value: 7, Op: specs.OpNotEqual}, seccomp.NotEqual, 7, true},
		{"less", specs.LinuxSeccompArg{Value: 9, Op: specs.OpLessThan}, seccomp.LessThan, 9, true},
		{"less or equal", specs.LinuxSeccompArg{Value: 9, Op: specs.OpLessEqual}, seccomp.LessOrEqual, 9, true},
		{"greater", specs.LinuxSeccompArg{Value: 9, Op: specs.OpGreaterThan}, seccomp.GreaterThan, 9, true},
		{"greater or equal", specs.LinuxSeccompArg{Value: 9, Op: specs.OpGreaterEqual}, seccomp.GreaterOrEqual, 9, true},
		{
			// the default profile's clone rule: deny namespace creation
			"masked eq, valuetwo 0 (default profile clone)",
			specs.LinuxSeccompArg{Index: 0, Value: nsFlags, ValueTwo: 0, Op: specs.OpMaskedEqual},
			seccomp.BitsNotSet, nsFlags, true,
		},
		{
			"masked eq, single-bit mask equals valuetwo",
			specs.LinuxSeccompArg{Value: 0x10000000, ValueTwo: 0x10000000, Op: specs.OpMaskedEqual},
			seccomp.BitsSet, 0x10000000, true,
		},
		{
			"masked eq, multi-bit mask equals valuetwo is not expressible",
			specs.LinuxSeccompArg{Value: nsFlags, ValueTwo: nsFlags, Op: specs.OpMaskedEqual},
			"", 0, false,
		},
		{
			"masked eq, valuetwo outside the mask is not expressible",
			specs.LinuxSeccompArg{Value: 0xff, ValueTwo: 0xf0, Op: specs.OpMaskedEqual},
			"", 0, false,
		},
		{"unknown operator", specs.LinuxSeccompArg{Value: 1, Op: "bogus"}, "", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			op, val, ok := bpfCondition(tc.arg)
			if ok != tc.ok || op != tc.op || val != tc.val {
				t.Fatalf("bpfCondition(%+v) = (%q, %d, %v), want (%q, %d, %v)",
					tc.arg, op, val, ok, tc.op, tc.val, tc.ok)
			}
		})
	}
}
