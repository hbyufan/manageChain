/*
Copyright IBM Corp. 2016 All Rights Reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

		 http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package ecies

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"errors"
	"io"
	"crypto/subtle"
	"fmt"
	"crypto/x509"
	"crypto/sha256"
	"golang.org/x/crypto/hkdf"
	"github.com/hyperledger/fabric/sm/sm4"
	"github.com/hyperledger/fabric/sm/sm3"
)

func aesEncrypt(key, plain []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}

	text := make([]byte, aes.BlockSize+len(plain))
	iv := text[:aes.BlockSize]
	if _, err := io.ReadFull(rand.Reader, iv); err != nil {
		return nil, err
	}

	cfb := cipher.NewCFBEncrypter(block, iv)
	cfb.XORKeyStream(text[aes.BlockSize:], plain)

	return text, nil
}

func aesDecrypt(key, text []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}

	if len(text) < aes.BlockSize {
		return nil, errors.New("cipher text too short")
	}

	cfb := cipher.NewCFBDecrypter(block, text[:aes.BlockSize])
	plain := make([]byte, len(text)-aes.BlockSize)
	cfb.XORKeyStream(plain, text[aes.BlockSize:])

	return plain, nil
}

func smEncrypt(key,text []byte) ([]byte,error){
	return sm4.Encrypt(key,text)
}

func smDecrypt(key,text []byte) ([]byte,error){
	return sm4.Decrypt(key,text)
}

func eciesGenerateKey(curve elliptic.Curve,rand io.Reader) (*ecdsa.PrivateKey, error) {
	return ecdsa.GenerateKey(curve, rand)
}

func eciesEncrypt(rand io.Reader, pub *ecdsa.PublicKey, s1, s2 []byte, plain []byte,usesm bool) ([]byte, error) {
	params := pub.Curve

	hash := sha256.New
	if usesm {
		hash = sm3.New
	}

	// Select an ephemeral elliptic curve key pair associated with
	// elliptic curve domain parameters params
	priv, Rx, Ry, err := elliptic.GenerateKey(pub.Curve, rand)
	//fmt.Printf("Rx %s\n", utils.EncodeBase64(Rx.Bytes()))
	//fmt.Printf("Ry %s\n", utils.EncodeBase64(Ry.Bytes()))

	// Convert R=(Rx,Ry) to an octed string R bar
	// This is uncompressed
	Rb := elliptic.Marshal(pub.Curve, Rx, Ry)

	// Derive a shared secret field element z from the ephemeral secret key k
	// and convert z to an octet string Z
	z, _ := params.ScalarMult(pub.X, pub.Y, priv)
	Z := z.Bytes()
	//fmt.Printf("Z %s\n", utils.EncodeBase64(Z))

	// generate keying data K of length ecnKeyLen + macKeyLen octects from Z
	// ans s1
	kELength := 32
	if usesm {
		kELength = 16
	}
	kE := make([]byte, kELength)
	kM := make([]byte, 32)
	hkdf := hkdf.New(hash, Z, s1, nil)
	_, err = hkdf.Read(kE)
	if err != nil {
		return nil, err
	}
	_, err = hkdf.Read(kM)
	if err != nil {
		return nil, err
	}

	// Use the encryption operation of the symmetric encryption scheme
	// to encrypt m under EK as ciphertext EM
	var EM []byte
	if !usesm{
		EM, err = aesEncrypt(kE, plain)
	}else{
		EM, err = smEncrypt(kE, plain)
	}
	// Use the tagging operation of the MAC scheme to compute
	// the tag D on EM || s2
	mac := hmac.New(hash, kM)
	mac.Write(EM)
	if len(s2) > 0 {
		mac.Write(s2)
	}
	D := mac.Sum(nil)

	// Output R,EM,D
	ciphertext := make([]byte, len(Rb)+len(EM)+len(D))
	//fmt.Printf("Rb %s\n", utils.EncodeBase64(Rb))
	//fmt.Printf("EM %s\n", utils.EncodeBase64(EM))
	//fmt.Printf("D %s\n", utils.EncodeBase64(D))
	copy(ciphertext, Rb)
	copy(ciphertext[len(Rb):], EM)
	copy(ciphertext[len(Rb)+len(EM):], D)

	return ciphertext, nil
}

func eciesDecrypt(priv *ecdsa.PrivateKey, s1, s2 []byte, ciphertext []byte,usesm bool) ([]byte, error) {
	params := priv.Curve
	hash := sha256.New
	if usesm{
		hash = sm3.New
	}

	var (
		rLen   int
		hLen   = hash().Size()
		mStart int
		mEnd   int
	)

	switch ciphertext[0] {
	case 2, 3:
		rLen = ((priv.PublicKey.Curve.Params().BitSize + 7) / 8) + 1
		if len(ciphertext) < (rLen + hLen + 1) {
			return nil, fmt.Errorf("Invalid ciphertext len [First byte = %d]", ciphertext[0])
		}
		break
	case 4:
		rLen = 2*((priv.PublicKey.Curve.Params().BitSize+7)/8) + 1
		if len(ciphertext) < (rLen + hLen + 1) {
			return nil, fmt.Errorf("Invalid ciphertext len [First byte = %d]", ciphertext[0])
		}
		break

	default:
		return nil, fmt.Errorf("Invalid ciphertext. Invalid first byte. [%d]", ciphertext[0])
	}

	mStart = rLen
	mEnd = len(ciphertext) - hLen
	//fmt.Printf("Rb %s\n", utils.EncodeBase64(ciphertext[:rLen]))

	Rx, Ry := elliptic.Unmarshal(priv.Curve, ciphertext[:rLen])
	if Rx == nil {
		return nil, errors.New("Invalid ephemeral PK")
	}
	if !priv.Curve.IsOnCurve(Rx, Ry) {
		return nil, errors.New("Invalid point on curve")
	}
	//fmt.Printf("Rx %s\n", utils.EncodeBase64(Rx.Bytes()))
	//fmt.Printf("Ry %s\n", utils.EncodeBase64(Ry.Bytes()))

	// Derive a shared secret field element z from the ephemeral secret key k
	// and convert z to an octet string Z
	z, _ := params.ScalarMult(Rx, Ry, priv.D.Bytes())
	Z := z.Bytes()
	//fmt.Printf("Z %s\n", utils.EncodeBase64(Z))

	// generate keying data K of length ecnKeyLen + macKeyLen octects from Z
	// ans s1
	kELength := 32
	if usesm{
		kELength = 16
	}
	kE := make([]byte, kELength)
	kM := make([]byte, 32)
	hkdf := hkdf.New(hash, Z, s1, nil)
	_, err := hkdf.Read(kE)
	if err != nil {
		return nil, err
	}
	_, err = hkdf.Read(kM)
	if err != nil {
		return nil, err
	}

	// Use the tagging operation of the MAC scheme to compute
	// the tag D on EM || s2 and then compare
	mac := hmac.New(hash,kM)
	mac.Write(ciphertext[mStart:mEnd])
	if len(s2) > 0 {
		mac.Write(s2)
	}
	D := mac.Sum(nil)

	//fmt.Printf("EM %s\n", utils.EncodeBase64(ciphertext[mStart:mEnd]))
	//fmt.Printf("D' %s\n", utils.EncodeBase64(D))
	//fmt.Printf("D %s\n", utils.EncodeBase64(ciphertext[mEnd:]))
	if subtle.ConstantTimeCompare(ciphertext[mEnd:], D) != 1 {
		return nil, errors.New("Tag check failed")
	}

	// Use the decryption operation of the symmetric encryption scheme
	// to decryptr EM under EK as plaintext
	var plaintext []byte
	if !usesm{
		plaintext, err = aesDecrypt(kE, ciphertext[mStart:mEnd])
	}else{
		plaintext,err = smDecrypt(kE,ciphertext[mStart:mEnd])
	}
	return plaintext, err
}

func EciesEncrypt(pub *ecdsa.PublicKey,msg []byte,usesm bool) ([]byte,error){
	return eciesEncrypt(rand.Reader,pub,nil,nil,msg,usesm)
}

func EciesDecrypt(priv *ecdsa.PrivateKey,ciphertext []byte,usesm bool) ([]byte,error){
	return eciesDecrypt(priv,nil,nil,ciphertext,usesm)
}

func ParseECPrivateKey(kb []byte) (*ecdsa.PrivateKey,error){
	return x509.ParseECPrivateKey(kb)
}

func ParseECPublicKey(kb []byte) (*ecdsa.PublicKey,error){
	pub,err := x509.ParsePKIXPublicKey(kb)
	return pub.(*ecdsa.PublicKey),err
}
/*
func main(){
	//rand.Reader
	priv, err := eciesGenerateKey(elliptic.P256(),rand.Reader)
	if err != nil{
		fmt.Printf("[hzyangwenlong] the generate key pair err :%v",err)
	}
	fmt.Printf("%v-->%v\n",priv,priv.PublicKey)
	msg := []byte("helloworld")
	encrypt,err := eciesEncrypt(rand.Reader, &(priv.PublicKey), nil, nil, msg)
	if err != nil{
		fmt.Printf("[hzyangwenlong] the encrypt err %v\n",err)
	}
	decrypt,err := eciesDecrypt(priv, nil, nil, encrypt)
	if err != nil{
		fmt.Printf("[hzyangwenlong] the decrypt err %v\n",err)
	}
	fmt.Println(string(encrypt))
	fmt.Println(string(decrypt))
}
*/
