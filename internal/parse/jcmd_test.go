package parse

import "testing"

func TestParseVMFlags(t *testing.T) {
	// JDK 25.0.5, Linux, -XX:+UseSerialGC -Xmx256m (shortened).
	f := ParseVMFlags("-XX:-AOTInvokeDynamicLinking -XX:CICompilerCount=12 -XX:InitialHeapSize=255852544 -XX:MaxHeapSize=268435456 -XX:+SegmentedCodeCache -XX:+UseSerialGC -XX:X86ICacheSync=3 \n")
	want := map[string]string{"AOTInvokeDynamicLinking": "false", "CICompilerCount": "12", "InitialHeapSize": "255852544",
		"MaxHeapSize": "268435456", "SegmentedCodeCache": "true", "UseSerialGC": "true", "X86ICacheSync": "3"}
	if len(f) != len(want) {
		t.Errorf("%v", f)
	}
	for k, v := range want {
		if f[k] != v {
			t.Errorf("%s = %q, want %q", k, f[k], v)
		}
	}
}

func TestParseHeapInfo(t *testing.T) {
	k := int64(1024)
	m := 1024 * k
	cases := []struct {
		name, text           string
		used, committed, max int64 // max -1 = nil
	}{
		{"serial", "DefNew     total 74944K, used 11992K [0x00000000f0000000, 0x00000000f5150000, 0x00000000f5550000)\n" +
			" eden space 66624K,  18% used [0x00000000f0000000, 0x00000000f0bb62e8, 0x00000000f4110000)\n" +
			"Tenured    total 166592K, used 1089K [0x00000000f5550000, 0x00000000ff800000, 0x0000000100000000)\n" +
			" the  space 166592K,   0% used [0x00000000f5550000, 0x00000000f5660548, 0x00000000ff800000)\n", (11992 + 1089) * k, (74944 + 166592) * k, -1},
		{"parallel", "PSYoungGen      total 76288K, used 0K [0x00000000fab00000, 0x0000000100000000, 0x0000000100000000)\n" +
			" eden space 65536K, 0% used [0x00000000fab00000,0x00000000fab00000,0x00000000feb00000)\n" +
			"ParOldGen       total 175104K, used 3170K [0x00000000f0000000, 0x00000000fab00000, 0x00000000fab00000)\n" +
			" object space 175104K, 1% used [0x00000000f0000000,0x00000000f0318bf8,0x00000000fab00000)\n" +
			"Metaspace       used 3364K, committed 3520K, reserved 1114112K\n", 3170 * k, (76288 + 175104) * k, -1},
		{"g1", "garbage-first heap   total reserved 262144K, committed 14336K, used 3170K [0x00000000f0000000, 0x0000000100000000)\n" +
			" region size 1024K, 0 young (0K), 0 survivors (0K)\n", 3170 * k, 14336 * k, 262144 * k},
		{"g1-old", "garbage-first heap   total 262144K, used 23360K [0x00000000f0000000, 0x0000000100000000)\n", 23360 * k, 262144 * k, -1},
		{"zgc", " ZHeap            used 6M, capacity 256M, max capacity 256M\n Cache           250M (3)\n", 6 * m, 256 * m, 256 * m},
		{"shenandoah", "Shenandoah Heap\n 256M max, 256M soft max, 256M committed, 3777K used\n 1024 x 256 K regions\n", 3777 * k, 256 * m, 256 * m},
	}
	for _, c := range cases {
		h, ok := ParseHeapInfo(c.text)
		if !ok || h.Used != c.used || h.Committed == nil || *h.Committed != c.committed || (c.max < 0) != (h.Max == nil) || (h.Max != nil && *h.Max != c.max) {
			t.Errorf("%s: %+v %v", c.name, h, ok)
		}
	}
	if _, ok := ParseHeapInfo("Unknown output\n"); ok {
		t.Error("unknown output must fail")
	}
	ms, ok := ParseMetaspace(cases[1].text)
	if !ok || ms.Used != 3364*k || ms.Committed != 3520*k || ms.Reserved != 1114112*k {
		t.Errorf("metaspace in heap_info: %+v", ms)
	}
	ms, ok = ParseMetaspace("Metaspace        used 1379K, committed 1536K, reserved 1114112K\n class space     used 139K, committed 192K, reserved 1048576K\n")
	if !ok || ms.Used != 1379*k {
		t.Errorf("VM.metaspace: %+v", ms)
	}
	if _, ok := ParseMetaspace("nothing"); ok {
		t.Error("must fail")
	}
}
