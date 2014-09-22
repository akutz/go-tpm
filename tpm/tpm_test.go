// Copyright (c) 2014, Google Inc. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package tpm

import (
	"bytes"
	"crypto/rand"
	"io/ioutil"
	"os"
	"testing"
)

func TestReadPCR(t *testing.T) {
	// Try to read PCR 18. For this to work, you have to have access to
	// /dev/tpm0, and there has to be a TPM driver to answer requests.
	f, err := os.OpenFile("/dev/tpm0", os.O_RDWR, 0600)
	defer f.Close()
	if err != nil {
		t.Fatal("Can't open /dev/tpm0 for read/write:", err)
	}

	res, err := ReadPCR(f, 18)
	if err != nil {
		t.Fatal("Couldn't read PCR 18 from the TPM:", err)
	}

	t.Logf("Got PCR 18 value % x\n", res)
}

func TestFetchPCRValues(t *testing.T) {
	f, err := os.OpenFile("/dev/tpm0", os.O_RDWR, 0600)
	defer f.Close()
	if err != nil {
		t.Fatal("Can't open /dev/tpm0 for read/write:", err)
	}

	var mask pcrMask
	if err := mask.setPCR(17); err != nil {
		t.Fatal("Couldn't set PCR 17:", err)
	}

	pcrs, err := FetchPCRValues(f, []int{17})
	if err != nil {
		t.Fatal("Couldn't get PCRs 17:", err)
	}

	comp, err := createPCRComposite(mask, pcrs)
	if err != nil {
		t.Fatal("Couldn't create PCR composite")
	}

	if len(comp) != int(digestSize) {
		t.Fatal("Invalid PCR composite")
	}

	// Locality is apparently always set to 0 in vTCIDirect.
	var locality byte
	_, err = createPCRInfoLong(locality, mask, pcrs)
	if err != nil {
		t.Fatal("Couldn't create a pcrInfoLong structure for these PCRs")
	}
}

func TestGetRandom(t *testing.T) {
	// Try to get 16 bytes of randomness from the TPM.
	f, err := os.OpenFile("/dev/tpm0", os.O_RDWR, 0600)
	defer f.Close()
	if err != nil {
		t.Fatal("Can't open /dev/tpm0 for read/write:", err)
	}

	b, err := GetRandom(f, 16)
	if err != nil {
		t.Fatal("Couldn't get 16 bytes of randomness from the TPM:", err)
	}

	t.Logf("Got random bytes % x\n", b)
}

func TestOIAP(t *testing.T) {
	f, err := os.OpenFile("/dev/tpm0", os.O_RDWR, 0600)
	defer f.Close()
	if err != nil {
		t.Fatal("Can't open /dev/tpm0 for read/write:", err)
	}

	// Get auth info from OIAP.
	resp, err := oiap(f)
	if err != nil {
		t.Fatal("Couldn't run OIAP:", err)
	}

	t.Logf("From OIAP, got AuthHandle %d and NonceEven % x\n", resp.AuthHandle, resp.NonceEven)
}

func TestOSAP(t *testing.T) {
	f, err := os.OpenFile("/dev/tpm0", os.O_RDWR, 0600)
	defer f.Close()
	if err != nil {
		t.Fatal("Can't open /dev/tpm0 for read/write:", err)
	}

	// Try to run OSAP for the SRK.
	osapc := &osapCommand{
		EntityType:  etSRK,
		EntityValue: khSRK,
	}

	if _, err := rand.Read(osapc.OddOSAP[:]); err != nil {
		t.Fatal("Couldn't get a random odd OSAP nonce")
	}

	resp, err := osap(f, osapc)
	if err != nil {
		t.Fatal("Couldn't run OSAP:", err)
	}

	t.Logf("From OSAP, go AuthHandle %d and NonceEven % x and EvenOSAP % x\n", resp.AuthHandle, resp.NonceEven, resp.EvenOSAP)
}

func TestResizeableSlice(t *testing.T) {
	// Set up an encoded slice with a byte array.
	ra := &responseAuth{
		NonceEven:   [20]byte{},
		ContSession: 1,
		Auth:        [20]byte{},
	}

	b := make([]byte, 322)
	if _, err := rand.Read(b); err != nil {
		t.Fatal("Couldn't read random bytes into the byte array")
	}

	rh := &responseHeader{
		Tag:  tagRSPAuth1Command,
		Size: 0,
		Res:  0,
	}

	in := []interface{}{rh, ra, b}
	rh.Size = uint32(packedSize(in))
	bb, err := pack(in)
	if err != nil {
		t.Fatal("Couldn't pack the bytes:", err)
	}

	var rh2 responseHeader
	var ra2 responseAuth
	var b2 []byte
	out := []interface{}{&rh2, &ra2, &b2}
	if err := unpack(bb, out); err != nil {
		t.Fatal("Couldn't unpack the resizeable values:", err)
	}

	if !bytes.Equal(b2, b) {
		t.Fatal("ResizeableSlice was not resized or copied correctly")
	}
}

func TestSeal(t *testing.T) {
	f, err := os.OpenFile("/dev/tpm0", os.O_RDWR, 0600)
	defer f.Close()
	if err != nil {
		t.Fatal("Can't open /dev/tpm0 for read/write:", err)
	}

	// Seal the same data as vTCIDirect so we can check the output as exactly as
	// possible.
	data := make([]byte, 64)
	data[0] = 1
	data[1] = 27
	data[2] = 52

	// The SRK auth is 20 bytes of zero for the well-known auth case.
	var srkAuth [20]byte
	sealed, err := Seal(f, 0 /* locality 0 */, []int{17} /* PCR 17 */, data, srkAuth[:])
	if err != nil {
		t.Fatal("Couldn't seal the data:", err)
	}

	data2, err := Unseal(f, sealed, srkAuth[:])
	if err != nil {
		t.Fatal("Couldn't unseal the data:", err)
	}

	if !bytes.Equal(data2, data) {
		t.Fatal("Unsealed data doesn't match original data")
	}
}

func TestLoadKey2(t *testing.T) {
	f, err := os.OpenFile("/dev/tpm0", os.O_RDWR, 0600)
	defer f.Close()
	if err != nil {
		t.Fatal("Can't open /dev/tpm0 for read/write:", err)
	}

	// Get the key from aikblob, assuming it exists. Otherwise, skip the test.
	blob, err := ioutil.ReadFile("./aikblob")
	if err != nil {
		t.Skip("No aikblob file; skipping test")
	}

	// We're using the well-known authenticator of 20 bytes of zeros.
	var srkAuth [20]byte
	handle, err := LoadKey2(f, blob, srkAuth[:])
	if err != nil {
		t.Fatal("Couldn't load the AIK into the TPM and get a handle for it:", err)
	}

	t.Logf("Loaded the AIK with handle %x\n", handle)
}

func TestQuote2(t *testing.T) {
	f, err := os.OpenFile("/dev/tpm0", os.O_RDWR, 0600)
	defer f.Close()
	if err != nil {
		t.Fatal("Can't open /dev/tpm0 for read/write:", err)
	}

	// Get the key from aikblob, assuming it exists. Otherwise, skip the test.
	blob, err := ioutil.ReadFile("./aikblob")
	if err != nil {
		t.Skip("No aikblob file; skipping test")
	}

	// Load the AIK for the quote.
	// We're using the well-known authenticator of 20 bytes of zeros.
	var srkAuth [20]byte
	handle, err := LoadKey2(f, blob, srkAuth[:])
	if err != nil {
		t.Fatal("Couldn't load the AIK into the TPM and get a handle for it:", err)
	}

	// Data to quote.
	data := []byte(`The OS says this test is good`)
	q, err := Quote2(f, handle, data, []int{17, 18}, 1 /* addVersion */, srkAuth[:])
	if err != nil {
		t.Fatal("Couldn't quote the data:", err)
	}

	t.Logf("Got a quote of length %d\n", len(q))
}

func TestGetPubKey(t *testing.T) {
	// For testing purposes, use the aikblob if it exists. Otherwise, just skip
	// this test. TODO(tmroeder): implement AIK creation so we can always run
	// this test.
	f, err := os.OpenFile("/dev/tpm0", os.O_RDWR, 0600)
	defer f.Close()
	if err != nil {
		t.Fatal("Can't open /dev/tpm0 for read/write:", err)
	}

	// Get the key from aikblob, assuming it exists. Otherwise, skip the test.
	blob, err := ioutil.ReadFile("./aikblob")
	if err != nil {
		t.Skip("No aikblob file; skipping test")
	}

	// Load the AIK for the quote.
	// We're using the well-known authenticator of 20 bytes of zeros.
	var srkAuth [20]byte
	handle, err := LoadKey2(f, blob, srkAuth[:])
	if err != nil {
		t.Fatal("Couldn't load the AIK into the TPM and get a handle for it:", err)
	}

	k, err := GetPubKey(f, handle, srkAuth[:])
	if err != nil {
		t.Fatal("Couldn't get the pub key for the AIK")
	}

	t.Logf("Got a pubkey blob of size %d\n", len(k))
}

func TestQuote(t *testing.T) {
	f, err := os.OpenFile("/dev/tpm0", os.O_RDWR, 0600)
	defer f.Close()
	if err != nil {
		t.Fatal("Can't open /dev/tpm0 for read/write:", err)
	}

	// Get the key from aikblob, assuming it exists. Otherwise, skip the test.
	blob, err := ioutil.ReadFile("./aikblob")
	if err != nil {
		t.Skip("No aikblob file; skipping test")
	}

	// Load the AIK for the quote.
	// We're using the well-known authenticator of 20 bytes of zeros.
	var srkAuth [20]byte
	handle, err := LoadKey2(f, blob, srkAuth[:])
	if err != nil {
		t.Fatal("Couldn't load the AIK into the TPM and get a handle for it:", err)
	}

	// Data to quote.
	data := []byte(`The OS says this test is good`)
	pcrNums := []int{17, 18}
	q, values, err := Quote(f, handle, data, pcrNums, srkAuth[:])
	if err != nil {
		t.Fatal("Couldn't quote the data:", err)
	}

	t.Logf("Got a quote of length %d\n", len(q))

	// Verify the quote.
	pk, err := UnmarshalRSAPublicKey(blob)
	if err != nil {
		t.Fatal("Couldn't extract an RSA key from the AIK blob:", err)
	}

	if err := VerifyQuote(pk, data, q, pcrNums, values); err != nil {
		t.Fatal("The quote didn't pass verification:", err)
	}
}

func TestUnmarshalRSAPublicKey(t *testing.T) {
	// Get the key from aikblob, assuming it exists. Otherwise, skip the test.
	blob, err := ioutil.ReadFile("./aikblob")
	if err != nil {
		t.Skip("No aikblob file; skipping test")
	}

	if _, err := UnmarshalRSAPublicKey(blob); err != nil {
		t.Fatal("Couldn't extract an RSA key from the AIK blob:", err)
	}
}

func TestMakeIdentity(t *testing.T) {
	f, err := os.OpenFile("/dev/tpm0", os.O_RDWR, 0600)
	defer f.Close()
	if err != nil {
		t.Fatal("Can't open /dev/tpm0 for read/write:", err)
	}

	// This test assumes that srkAuth and ownerAuth are the well-known zero
	// secrets. It also only tests the case of setting AIK auth to a well-known
	// 0 secret.
	var srkAuth digest
	var ownerAuth digest
	var aikAuth digest

	// In the simplest case, we pass in nil for the Privacy CA key and the
	// label.
	blob, err := MakeIdentity(f, srkAuth[:], ownerAuth[:], aikAuth[:], nil, nil)
	if err != nil {
		t.Fatal("Couldn't make a new AIK in the TPM:", err)
	}

	t.Logf("Got a new AIK blob of length %d\n", len(blob))
	handle, err := LoadKey2(f, blob, srkAuth[:])
	if err != nil {
		t.Fatal("Couldn't load the freshly-generated AIK into the TPM and get a handle for it:", err)
	}
	t.Logf("Got AIK handle %d\n", handle)

	// Data to quote.
	data := []byte(`The OS says this test and new AIK is good`)
	pcrNums := []int{17, 18}
	q, values, err := Quote(f, handle, data, pcrNums, srkAuth[:])
	if err != nil {
		t.Fatal("Couldn't quote the data:", err)
	}

	t.Logf("Got a quote of length %d\n", len(q))

	// Verify the quote.
	pk, err := UnmarshalRSAPublicKey(blob)
	if err != nil {
		t.Fatal("Couldn't extract an RSA key from the AIK blob:", err)
	}

	if err := VerifyQuote(pk, data, q, pcrNums, values); err != nil {
		t.Fatal("The quote didn't pass verification:", err)
	}
}

func TestResetLockValue(t *testing.T) {
	f, err := os.OpenFile("/dev/tpm0", os.O_RDWR, 0600)
	defer f.Close()
	if err != nil {
		t.Fatal("Can't open /dev/tpm0 for read/write:", err)
	}

	// This test code assumes that the owner auth is the well-known value.
	var ownerAuth digest
	if err := ResetLockValue(f, ownerAuth); err != nil {
		t.Fatal("Couldn't reset the lock value:", err)
	}
}