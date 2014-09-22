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

// Package tpm supports direct communication with a tpm device under Linux.
package tpm

import (
	"crypto"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"encoding/binary"
	"errors"
	"os"

	"github.com/golang/glog"
)

// ReadPCR reads a PCR value from the TPM.
func ReadPCR(f *os.File, pcr uint32) ([]byte, error) {
	in := []interface{}{pcr}
	var v pcrValue
	out := []interface{}{&v}
	// There's no need to check the ret value here, since the err value contains
	// all the necessary information.
	if _, err := submitTPMRequest(f, tagRQUCommand, ordPCRRead, in, out); err != nil {
		return nil, err
	}

	return v[:], nil
}

// FetchPCRValues gets a given sequence of PCR values.
func FetchPCRValues(f *os.File, pcrVals []int) ([]byte, error) {
	var pcrs []byte
	for _, v := range pcrVals {
		pcr, err := ReadPCR(f, uint32(v))
		if err != nil {
			return nil, err
		}

		pcrs = append(pcrs, pcr...)
	}

	return pcrs, nil
}

// GetRandom gets random bytes from the TPM.
func GetRandom(f *os.File, size uint32) ([]byte, error) {
	var b []byte
	in := []interface{}{size}
	out := []interface{}{&b}
	// There's no need to check the ret value here, since the err value
	// contains all the necessary information.
	if _, err := submitTPMRequest(f, tagRQUCommand, ordGetRandom, in, out); err != nil {
		return nil, err
	}

	return b, nil
}

// LoadKey2 loads a key blob (a serialized TPM_KEY or TPM_KEY12) into the TPM
// and returns a handle for this key.
func LoadKey2(f *os.File, keyBlob []byte, srkAuth []byte) (Handle, error) {
	// Deserialize the keyBlob as a key
	var k key
	if err := unpack(keyBlob, []interface{}{&k}); err != nil {
		return 0, err
	}

	if glog.V(2) {
		glog.Infof("Unpacked the key as %+v\n", k)
	}

	// Run OSAP for the SRK, reading a random OddOSAP for our initial
	// command and getting back a secret and a handle. LoadKey2 needs an
	// OSAP session for the SRK because the private part of a TPM_KEY or
	// TPM_KEY12 is sealed against the SRK.
	sharedSecret, osapr, err := newOSAPSession(f, etSRK, khSRK, srkAuth)
	if err != nil {
		return 0, err
	}
	defer osapr.Close(f)
	defer zeroBytes(sharedSecret[:])

	authIn := []interface{}{ordLoadKey2, k}
	ca, err := newCommandAuth(osapr.AuthHandle, osapr.NonceEven, sharedSecret[:], authIn)
	if err != nil {
		return 0, err
	}

	if glog.V(2) {
		glog.Info("About to load the key")
	}

	handle, ra, ret, err := loadKey2(f, &k, ca)
	if err != nil {
		return 0, err
	}

	// Check the response authentication.
	raIn := []interface{}{ret, ordLoadKey2}
	if err := ra.verify(ca.NonceOdd, sharedSecret[:], raIn); err != nil {
		return 0, err
	}

	return handle, nil
}

// Quote2 performs a quote operation on the TPM for the given data,
// under the key associated with the handle and for the pcr values
// specified in the call.
func Quote2(f *os.File, handle Handle, data []byte, pcrVals []int, addVersion byte, srkAuth []byte) ([]byte, error) {
	// Run OSAP for the handle, reading a random OddOSAP for our initial
	// command and getting back a secret and a response.
	sharedSecret, osapr, err := newOSAPSession(f, etKeyHandle, handle, srkAuth)
	if err != nil {
		return nil, err
	}
	defer osapr.Close(f)
	defer zeroBytes(sharedSecret[:])

	// Hash the data to get the value to pass to quote2.
	hash := sha1.Sum(data)
	pcrSel, err := newPCRSelection(pcrVals)
	if err != nil {
		return nil, err
	}
	authIn := []interface{}{ordQuote2, hash, pcrSel, addVersion}
	ca, err := newCommandAuth(osapr.AuthHandle, osapr.NonceEven, sharedSecret[:], authIn)
	if err != nil {
		return nil, err
	}

	// TODO(tmroeder): use the returned capVersionInfo.
	pcrShort, _, capBytes, sig, ra, ret, err := quote2(f, handle, hash, pcrSel, addVersion, ca)
	if err != nil {
		return nil, err
	}

	// Check response authentication.
	raIn := []interface{}{ret, ordQuote2, pcrShort, capBytes, sig}
	if err := ra.verify(ca.NonceOdd, sharedSecret[:], raIn); err != nil {
		return nil, err
	}

	return sig, nil
}

// GetPubKey retrieves an opaque blob containing a public key corresponding to
// a handle from the TPM.
func GetPubKey(f *os.File, keyHandle Handle, srkAuth []byte) ([]byte, error) {
	// Run OSAP for the handle, reading a random OddOSAP for our initial
	// command and getting back a secret and a response.
	sharedSecret, osapr, err := newOSAPSession(f, etKeyHandle, keyHandle, srkAuth)
	if err != nil {
		return nil, err
	}
	defer osapr.Close(f)
	defer zeroBytes(sharedSecret[:])

	authIn := []interface{}{ordGetPubKey}
	ca, err := newCommandAuth(osapr.AuthHandle, osapr.NonceEven, sharedSecret[:], authIn)
	if err != nil {
		return nil, err
	}

	pk, ra, ret, err := getPubKey(f, keyHandle, ca)
	if err != nil {
		return nil, err
	}

	// Check response authentication for TPM_GetPubKey.
	raIn := []interface{}{ret, ordGetPubKey, pk}
	if err := ra.verify(ca.NonceOdd, sharedSecret[:], raIn); err != nil {
		return nil, err
	}

	b, err := pack([]interface{}{*pk})
	if err != nil {
		return nil, err
	}
	return b, err
}

// newOSAPSession starts a new OSAP session and derives a shared key from it.
func newOSAPSession(f *os.File, entityType uint16, entityValue Handle, srkAuth []byte) ([20]byte, *osapResponse, error) {
	osapc := &osapCommand{
		EntityType:  entityType,
		EntityValue: entityValue,
	}

	var sharedSecret [20]byte
	if _, err := rand.Read(osapc.OddOSAP[:]); err != nil {
		return sharedSecret, nil, err
	}
	if glog.V(2) {
		glog.Infof("osapCommand is %s\n", osapc)
	}

	osapr, err := osap(f, osapc)
	if err != nil {
		return sharedSecret, nil, err
	}
	if glog.V(2) {
		glog.Infof("osapResponse is %s\n", osapr)
	}

	// A shared secret is computed as
	//
	// sharedSecret = HMAC-SHA1(srkAuth, evenosap||oddosap)
	//
	// where srkAuth is the hash of the SRK authentication (which hash is all 0s
	// for the well-known SRK auth value) and even and odd OSAP are the
	// values from the OSAP protocol.
	osapData, err := pack([]interface{}{osapr.EvenOSAP, osapc.OddOSAP})
	if err != nil {
		return sharedSecret, nil, err
	}

	if glog.V(2) {
		glog.Infof("osapData is % x\n", osapData)
	}

	hm := hmac.New(sha1.New, srkAuth)
	hm.Write(osapData)
	// Note that crypto/hash.Sum returns a slice rather than an array, so we
	// have to copy this into an array to make sure that serialization doesn't
	// preprend a length in pack().
	sharedSecretBytes := hm.Sum(nil)
	copy(sharedSecret[:], sharedSecretBytes)

	if glog.V(2) {
		glog.Infof("hmac size is %d\n", hm.Size())
		glog.Infof("sharedSecret is % x\n", sharedSecret)
		glog.Infof("length of shared secret is %d\n", len(sharedSecret))
	}

	return sharedSecret, osapr, nil
}

// newCommandAuth creates a new commandAuth structure over the given
// parameters, using the given secret for HMAC computation.
func newCommandAuth(authHandle Handle, nonceEven nonce, key []byte, params []interface{}) (*commandAuth, error) {
	// Auth = HMAC-SHA1(key, SHA1(params) || NonceEven || NonceOdd || ContSession)
	digestBytes, err := pack(params)
	if err != nil {
		return nil, err
	}
	if glog.V(2) {
		glog.Infof("digestBytes is % x\n", digestBytes)
	}

	digest := sha1.Sum(digestBytes)
	if glog.V(2) {
		glog.Infof("digest is % x\n", digest)
	}

	ca := &commandAuth{AuthHandle: authHandle}
	if _, err := rand.Read(ca.NonceOdd[:]); err != nil {
		return nil, err
	}
	if glog.V(2) {
		glog.Infof("commandAuth is %s\n", ca)
	}

	authBytes, err := pack([]interface{}{digest, nonceEven, ca.NonceOdd, ca.ContSession})
	if err != nil {
		return nil, err
	}
	if glog.V(2) {
		glog.Infof("authBytes is % x\n", authBytes)
	}

	hm2 := hmac.New(sha1.New, key)
	hm2.Write(authBytes)
	auth := hm2.Sum(nil)
	copy(ca.Auth[:], auth[:])
	if glog.V(2) {
		glog.Infof("commandAuth now is %s\n", ca)
	}

	return ca, nil
}

// verify checks that the response authentication was correct.
// It computes the SHA1 of params, and computes the HMAC-SHA1 of this digest
// with the authentication parameters of ra along with the given odd nonce.
func (ra *responseAuth) verify(nonceOdd nonce, key []byte, params []interface{}) error {
	// Auth = HMAC-SHA1(key, SHA1(params) || ra.NonceEven || NonceOdd || ra.ContSession)
	digestBytes, err := pack(params)
	if err != nil {
		return err
	}
	if glog.V(2) {
		glog.Infof("response digestBytes is % x\n", digestBytes)
	}

	digest := sha1.Sum(digestBytes)
	if glog.V(2) {
		glog.Infof("response digest is % x\n", digest)
	}

	authBytes, err := pack([]interface{}{digest, ra.NonceEven, nonceOdd, ra.ContSession})
	if err != nil {
		return err
	}
	if glog.V(2) {
		glog.Infof("response authBytes is % x\n", authBytes)
	}

	hm2 := hmac.New(sha1.New, key)
	hm2.Write(authBytes)
	auth := hm2.Sum(nil)

	if !hmac.Equal(ra.Auth[:], auth) {
		return errors.New("the computed response HMAC didn't match the provided HMAC")
	}

	return nil
}

// zeroBytes zeroes a byte array.
func zeroBytes(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

// Seal encrypts data against a given locality and PCRs and returns the sealed data.
func Seal(f *os.File, locality byte, pcrs []int, data []byte, srkAuth []byte) ([]byte, error) {
	pcrInfo, err := newPCRInfoLong(f, locality, pcrs)
	if err != nil {
		return nil, err
	}
	if glog.V(2) {
		glog.Infof("pcrInfo is %s\n", pcrInfo)
	}

	// Run OSAP for the SRK, reading a random OddOSAP for our initial
	// command and getting back a secret and a handle.
	sharedSecret, osapr, err := newOSAPSession(f, etSRK, khSRK, srkAuth)
	if err != nil {
		return nil, err
	}
	defer osapr.Close(f)
	defer zeroBytes(sharedSecret[:])

	// EncAuth for a seal command is computed as
	//
	// encAuth = XOR(srkAuth, SHA1(sharedSecret || <lastEvenNonce>))
	//
	// In this case, the last even nonce is NonceEven from OSAP.
	xorData, err := pack([]interface{}{sharedSecret, osapr.NonceEven})
	if err != nil {
		return nil, err
	}
	if glog.V(2) {
		glog.Infof("xorData is % x\n", xorData)
	}
	defer zeroBytes(xorData)

	encAuthData := sha1.Sum(xorData)
	if glog.V(2) {
		glog.Infof("encAuthData is % x\n", encAuthData)
	}

	sc := &sealCommand{KeyHandle: khSRK}
	for i := range sc.EncAuth {
		sc.EncAuth[i] = srkAuth[i] ^ encAuthData[i]
	}
	if glog.V(2) {
		glog.Infof("sealCommand is %s\n", sc)
	}

	// The digest input for seal authentication is
	//
	// digest = SHA1(ordSeal || encAuth || binary.Size(pcrInfo) || pcrInfo ||
	//               len(data) || data)
	//
	authIn := []interface{}{ordSeal, sc.EncAuth, uint32(binary.Size(pcrInfo)), pcrInfo, data}
	ca, err := newCommandAuth(osapr.AuthHandle, osapr.NonceEven, sharedSecret[:], authIn)
	if err != nil {
		return nil, err
	}

	sealed, ra, ret, err := seal(f, sc, pcrInfo, data, ca)
	if err != nil {
		return nil, err
	}

	// Check the response authentication.
	raIn := []interface{}{ret, ordSeal, sealed}
	if err := ra.verify(ca.NonceOdd, sharedSecret[:], raIn); err != nil {
		return nil, err
	}

	sealedBytes, err := pack([]interface{}{*sealed})
	if err != nil {
		return nil, err
	}

	return sealedBytes, nil
}

// Unseal decrypts data encrypted by the TPM.
func Unseal(f *os.File, sealed []byte, srkAuth []byte) ([]byte, error) {
	// Run OSAP for the SRK, reading a random OddOSAP for our initial
	// command and getting back a secret and a handle.
	sharedSecret, osapr, err := newOSAPSession(f, etSRK, khSRK, srkAuth)
	if err != nil {
		return nil, err
	}
	defer osapr.Close(f)
	defer zeroBytes(sharedSecret[:])

	// The unseal command needs an OIAP session in addition to the OSAP session.
	oiapr, err := oiap(f)
	if err != nil {
		return nil, err
	}
	defer oiapr.Close(f)

	// Convert the sealed value into a tpmStoredData.
	var tsd tpmStoredData
	if err := unpack(sealed, []interface{}{&tsd}); err != nil {
		return nil, errors.New("couldn't convert the sealed data into a tpmStoredData struct")
	}
	if glog.V(2) {
		glog.Infof("tpmStoredData is %s\n", tsd)
	}

	// The digest for auth1 and auth2 for the unseal command is computed as
	// digest = SHA1(ordUnseal || tsd)
	authIn := []interface{}{ordUnseal, tsd}

	// The first commandAuth uses the shared secret as an HMAC key.
	ca1, err := newCommandAuth(osapr.AuthHandle, osapr.NonceEven, sharedSecret[:], authIn)
	if err != nil {
		return nil, err
	}

	// The second commandAuth is based on OIAP instead of OSAP and uses the
	// SRK auth value as an HMAC key instead of the shared secret.
	ca2, err := newCommandAuth(oiapr.AuthHandle, oiapr.NonceEven, srkAuth, authIn)
	if err != nil {
		return nil, err
	}

	unsealed, ra1, ra2, ret, err := unseal(f, khSRK, &tsd, ca1, ca2)
	if err != nil {
		return nil, err
	}

	// Check the response authentication.
	raIn := []interface{}{ret, ordUnseal, unsealed}
	if err := ra1.verify(ca1.NonceOdd, sharedSecret[:], raIn); err != nil {
		return nil, err
	}

	if err := ra2.verify(ca2.NonceOdd, srkAuth, raIn); err != nil {
		return nil, err
	}

	return unsealed, nil
}

func Quote(f *os.File, handle Handle, data []byte, pcrNums []int, srkAuth []byte) ([]byte, []byte, error) {
	// Run OSAP for the handle, reading a random OddOSAP for our initial
	// command and getting back a secret and a response.
	sharedSecret, osapr, err := newOSAPSession(f, etKeyHandle, handle, srkAuth)
	if err != nil {
		return nil, nil, err
	}
	defer osapr.Close(f)
	defer zeroBytes(sharedSecret[:])

	// Hash the data to get the value to pass to quote2.
	hash := sha1.Sum(data)
	pcrSel, err := newPCRSelection(pcrNums)
	if err != nil {
		return nil, nil, err
	}
	authIn := []interface{}{ordQuote, hash, pcrSel}
	ca, err := newCommandAuth(osapr.AuthHandle, osapr.NonceEven, sharedSecret[:], authIn)
	if err != nil {
		return nil, nil, err
	}

	pcrc, sig, ra, ret, err := quote(f, handle, hash, pcrSel, ca)
	if err != nil {
		return nil, nil, err
	}

	// Check response authentication.
	raIn := []interface{}{ret, ordQuote, pcrc, sig}
	if err := ra.verify(ca.NonceOdd, sharedSecret[:], raIn); err != nil {
		return nil, nil, err
	}

	return sig, pcrc.Values, nil
}

// MakeIdentity creates a new AIK with the given new auth value, and the given
// parameters for the privacy CA that will be used to attest to it.
// If both pk and label are nil, then the TPM_CHOSENID_HASH is set to all 0s as
// a special case. MakeIdentity returns a key blob for the newly-created key.
// The caller must be authorized to use the SRK, since the private part of the
// AIK is sealed against the SRK.
// TODO(tmroeder): currently, this code can only create 2048-bit RSA keys.
func MakeIdentity(f *os.File, srkAuth []byte, ownerAuth []byte, aikAuth []byte, pk crypto.PublicKey, label []byte) ([]byte, error) {
	// Run OSAP for the SRK, reading a random OddOSAP for our initial command
	// and getting back a secret and a handle.
	sharedSecretSRK, osaprSRK, err := newOSAPSession(f, etSRK, khSRK, srkAuth)
	if err != nil {
		return nil, err
	}
	defer osaprSRK.Close(f)
	defer zeroBytes(sharedSecretSRK[:])

	// Run OSAP for the Owner, reading a random OddOSAP for our initial command
	// and getting back a secret and a handle.
	sharedSecretOwn, osaprOwn, err := newOSAPSession(f, etOwner, khOwner, ownerAuth)
	if err != nil {
		return nil, err
	}
	defer osaprOwn.Close(f)
	defer zeroBytes(sharedSecretOwn[:])

	// EncAuth for a MakeIdentity command is computed as
	//
	// encAuth = XOR(aikAuth, SHA1(sharedSecretOwn || <lastEvenNonce>))
	//
	// In this case, the last even nonce is NonceEven from OSAP for the Owner.
	xorData, err := pack([]interface{}{sharedSecretOwn, osaprOwn.NonceEven})
	if err != nil {
		return nil, err
	}
	defer zeroBytes(xorData)

	encAuthData := sha1.Sum(xorData)
	var encAuth digest
	for i := range encAuth {
		encAuth[i] = aikAuth[i] ^ encAuthData[i]
	}

	var caDigest digest
	if (pk != nil) != (label != nil) {
		return nil, errors.New("inconsistent null values between the pk and the label")
	}

	if pk != nil {
		pubk, err := convertPubKey(pk)
		if err != nil {
			return nil, err
		}

		// We can't pack the pair of values directly, since the label is
		// included directly as bytes, without any length.
		fullpkb, err := pack([]interface{}{pubk})
		if err != nil {
			return nil, err
		}

		caDigestBytes := append(label, fullpkb...)
		caDigest = sha1.Sum(caDigestBytes)
	}

	rsaAIKParms := rsaKeyParms{
		KeyLength: 2048,
		NumPrimes: 2,
		//Exponent:  big.NewInt(0x10001).Bytes(), // 65537. Implicit?
	}
	packedParms, err := pack([]interface{}{rsaAIKParms})
	if err != nil {
		return nil, err
	}

	aikParms := keyParms{
		AlgID:     algRSA,
		EncScheme: esNone,
		SigScheme: ssRSASaPKCS1v15_SHA1,
		Parms:     packedParms,
	}

	aik := &key{
		Version:        0x01010000,
		KeyUsage:       keyIdentity,
		KeyFlags:       0,
		AuthDataUsage:  authAlways,
		AlgorithmParms: aikParms,
	}

	// The digest input for MakeIdentity authentication is
	//
	// digest = SHA1(ordMakeIdentity || encAuth || caDigest || aik)
	//
	authIn := []interface{}{ordMakeIdentity, encAuth, caDigest, aik}
	ca1, err := newCommandAuth(osaprSRK.AuthHandle, osaprSRK.NonceEven, sharedSecretSRK[:], authIn)
	if err != nil {
		return nil, err
	}

	ca2, err := newCommandAuth(osaprOwn.AuthHandle, osaprOwn.NonceEven, sharedSecretOwn[:], authIn)
	if err != nil {
		return nil, err
	}

	k, sig, ra1, ra2, ret, err := makeIdentity(f, encAuth, caDigest, aik, ca1, ca2)
	if err != nil {
		return nil, err
	}

	// Check response authentication.
	raIn := []interface{}{ret, ordMakeIdentity, k, sig}
	if err := ra1.verify(ca1.NonceOdd, sharedSecretSRK[:], raIn); err != nil {
		return nil, err
	}

	if err := ra2.verify(ca2.NonceOdd, sharedSecretOwn[:], raIn); err != nil {
		return nil, err
	}

	// TODO(tmroeder): check the signature against the pubek.
	blob, err := pack([]interface{}{k})
	if err != nil {
		return nil, err
	}

	return blob, nil
}

// ResetLockValue resets the dictionary-attack value in the TPM; this allows the
// TPM to start working again after authentication errors without waiting for
// the dictionary-attack defenses to time out. This requires owner
// authentication.
func ResetLockValue(f *os.File, ownerAuth digest) error {
	// Run OSAP for the Owner, reading a random OddOSAP for our initial command
	// and getting back a secret and a handle.
	sharedSecretOwn, osaprOwn, err := newOSAPSession(f, etOwner, khOwner, ownerAuth[:])
	if err != nil {
		return err
	}
	defer osaprOwn.Close(f)
	defer zeroBytes(sharedSecretOwn[:])

	// The digest input for MakeIdentity authentication is
	//
	// digest = SHA1(ordResetLockValue)
	//
	authIn := []interface{}{ordResetLockValue}
	ca, err := newCommandAuth(osaprOwn.AuthHandle, osaprOwn.NonceEven, sharedSecretOwn[:], authIn)
	if err != nil {
		return err
	}

	ra, ret, err := resetLockValue(f, ca)
	if err != nil {
		return err
	}

	// Check response authentication.
	raIn := []interface{}{ret, ordResetLockValue}
	if err := ra.verify(ca.NonceOdd, sharedSecretOwn[:], raIn); err != nil {
		return err
	}

	return nil
}