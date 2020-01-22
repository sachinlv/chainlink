// Package vrf provides a cryptographically secure pseudo-random number generator.
////////////////////////////////////////////////////////////////////////////////
//       XXX: Do not use in production until this code has been audited.
////////////////////////////////////////////////////////////////////////////////
// Numbers are deterministically generated from a seed and a secret key, and are
// statistically indistinguishable from uniform sampling from {0, ..., 2**256},
// to observers who don't know the key. But each number comes with a proof that
// it was generated according to the procedure mandated by a public key
// associated with that private key.
//
// See VRF.sol for design notes.
//
// Usage
// -----
//
// A secret key sk should be securely sampled uniformly from {0, ..., Order}.
// The public key associated with it can be calculated from it by
//
//   secp256k1.Secp256k1{}.Point().Mul(secureKey, Generator)
//
// To generate random output from a big.Int seed, pass sk and the seed to
// GenerateProof, and use the Output field of the returned Proof object.
//
// To verify a Proof object p, run p.Verify(); or to verify it on-chain pass
// p.MarshalForSolidityVerifier() to randomValueFromVRFProof on the VRF solidity
// contract.
package vrf

import (
	"crypto/rand"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/pkg/errors"

	"chainlink/core/services/signatures/secp256k1"
	"chainlink/core/utils"

	"go.dedis.ch/kyber/v3"
)

func bigFromHex(s string) *big.Int {
	n, ok := new(big.Int).SetString(s, 16)
	if !ok {
		panic(fmt.Errorf(`failed to convert "%s" as hex to big.Int`, s))
	}
	return n
}

// fieldSize is number of elements in secp256k1's base field, i.e. GF(fieldSize)
var fieldSize = bigFromHex(
	"FFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFEFFFFFC2F")

// Order is the number of rational points on the curve in GF(P) (group size)
var Order = secp256k1.GroupOrder

var bi = big.NewInt
var zero, one, two, three, four, seven = bi(0), bi(1), bi(2), bi(3), bi(4), bi(7)

// Compensate for awkward big.Int API. Can cause an extra allocation or two.
func i() *big.Int                                    { return new(big.Int) }
func add(addend1, addend2 *big.Int) *big.Int         { return i().Add(addend1, addend2) }
func div(dividend, divisor *big.Int) *big.Int        { return i().Div(dividend, divisor) }
func equal(left, right *big.Int) bool                { return left.Cmp(right) == 0 }
func exp(base, exponent, modulus *big.Int) *big.Int  { return i().Exp(base, exponent, modulus) }
func lsh(num *big.Int, bits uint) *big.Int           { return i().Lsh(num, bits) }
func mul(multiplicand, multiplier *big.Int) *big.Int { return i().Mul(multiplicand, multiplier) }
func mod(dividend, divisor *big.Int) *big.Int        { return i().Mod(dividend, divisor) }
func sub(minuend, subtrahend *big.Int) *big.Int      { return i().Sub(minuend, subtrahend) }

var (
	// (P-1)/2: Half Fermat's Little Theorem exponent
	eulersCriterionPower = div(sub(fieldSize, one), two)
	// (P+1)/4: As long as P%4==3 and n=x^2 in GF(P), n^((P+1)/4)=±x
	sqrtPower = div(add(fieldSize, one), four)
)

// IsSquare returns true iff x = y^2 for some y in GF(p)
func IsSquare(x *big.Int) bool {
	return equal(one, exp(x, eulersCriterionPower, fieldSize))
}

// SquareRoot returns a s.t. a^2=x. Assumes x is a square
func SquareRoot(x *big.Int) *big.Int {
	return exp(x, sqrtPower, fieldSize)
}

// YSquared returns x^3+7 mod P
func YSquared(x *big.Int) *big.Int {
	return mod(add(exp(x, three, fieldSize), seven), fieldSize)
}

// IsCurveXOrdinate returns true iff there is y s.t. y^2=x^3+7
func IsCurveXOrdinate(x *big.Int) bool {
	return IsSquare(YSquared(x))
}

// packUint256s returns xs serialized as concatenated uint256s, or an error
func packUint256s(xs ...*big.Int) ([]byte, error) {
	mem := []byte{}
	for _, x := range xs {
		word, err := utils.EVMWordBigInt(x)
		if err != nil {
			return []byte{}, errors.Wrap(err, "vrf.packUint256s#EVMWordBigInt")
		}
		mem = append(mem, word...)
	}
	return mem, nil
}

var secp256k1Curve = &secp256k1.Secp256k1{}

// Generator is the generator point of secp256k1
var Generator = secp256k1Curve.Point().Base()

// HashUint256s returns a uint256 representing the hash of the concatenated byte
// representations of the inputs
func HashUint256s(xs ...*big.Int) (*big.Int, error) {
	packed, err := packUint256s(xs...)
	if err != nil {
		return &big.Int{}, err
	}
	hash, err := utils.Keccak256(packed)
	if err != nil {
		return &big.Int{}, errors.Wrap(err, "vrf.HashUint256s#Keccak256")
	}
	return i().SetBytes(hash), nil
}

func uint256ToBytes32(x *big.Int) []byte {
	if x.BitLen() > 256 {
		panic("vrf.uint256ToBytes32: too big to marshal to uint256")
	}
	return common.LeftPadBytes(x.Bytes(), 32)
}

var zqHashPanicTemplate = "will only work for messages of at most that long, " +
	"but message is %d bits"

// ZqHash hashes xs uniformly into {0, ..., q-1}. q must be 256 bits long, and
// msg is assumed to already be a 256-bit hash
func ZqHash(msg []byte) (*big.Int, error) {
	if len(msg) > 32 {
		panic(fmt.Errorf(zqHashPanicTemplate, len(msg)*8))
	}
	rv := i().SetBytes(msg)
	// Hash recursively until rv < q. P(success per iteration) >= 0.5, so
	// number of extra hashes is geometrically distributed, with mean < 1.
	for rv.Cmp(fieldSize) >= 0 {
		rv.SetBytes(utils.MustHash(string(uint256ToBytes32(rv))).Bytes())
	}
	return rv, nil
}

func initialXOrdinate(p kyber.Point, input *big.Int) (*big.Int, error) {
	if !(secp256k1.ValidPublicKey(p) && input.BitLen() <= 256) {
		return nil, fmt.Errorf("bad input to vrf.HashToCurve")
	}
	iHash, err := utils.Keccak256(
		append(secp256k1.LongMarshal(p), uint256ToBytes32(input)...))
	if err != nil {
		return nil, errors.Wrap(err, "while attempting initial hash")
	}
	x, err := ZqHash(iHash)
	if err != nil {
		return nil, errors.Wrap(err, "vrf.HashToCurve#ZqHash")
	}
	return x, nil
}

// HashToCurve is a one-way hash function onto the curve. Returns the curve
// point and the y-ordinates computed in the process of finding the point, or an
// error. It passes each candidate x ordinate to ordinates.
func HashToCurve(p kyber.Point, input *big.Int, ordinates func(x *big.Int),
) (kyber.Point, error) {
	x, err := initialXOrdinate(p, input)
	if err != nil {
		return nil, err
	}
	ordinates(x)
	for !IsCurveXOrdinate(x) { // Hash recursively until x^3+7 is a square
		nHash, err := utils.Keccak256(uint256ToBytes32(x))
		if err != nil {
			return nil, errors.Wrap(err, "while attempting to rehash x")
		}
		nx, err := ZqHash(nHash)
		if err != nil {
			return nil, err
		}
		x.Set(nx)
		ordinates(x)
	}
	y := SquareRoot(YSquared(x))
	rv := secp256k1.SetCoordinates(x, y)
	if i().Mod(y, two).Cmp(one) == 0 { // Negate response if y odd
		rv = rv.Neg(rv)
	}
	return rv, nil
}

// ScalarFromCurvePoints returns a hash for the curve points. Corresponds to the
// hash computed in VRF.sol#scalarFromCurve
func ScalarFromCurvePoints(
	hash, pk, gamma kyber.Point, uWitness [20]byte, v kyber.Point) *big.Int {
	if !(secp256k1.ValidPublicKey(hash) && secp256k1.ValidPublicKey(pk) &&
		secp256k1.ValidPublicKey(gamma) && secp256k1.ValidPublicKey(v)) {
		panic("bad arguments to vrf.ScalarFromCurvePoints")
	}
	// msg will contain abi.encodePacked(hash, pk, gamma, v, uWitness)
	msg := secp256k1.LongMarshal(hash)
	msg = append(msg, secp256k1.LongMarshal(pk)...)
	msg = append(msg, secp256k1.LongMarshal(gamma)...)
	msg = append(msg, secp256k1.LongMarshal(v)...)
	msg = append(msg, uWitness[:]...)
	return i().SetBytes(utils.MustHash(string(msg)).Bytes())
}

// linearComination returns c*p1+s*p2
func linearCombination(c *big.Int, p1 kyber.Point,
	s *big.Int, p2 kyber.Point) kyber.Point {
	return secp256k1Curve.Point().Add(
		secp256k1Curve.Point().Mul(secp256k1.IntToScalar(c), p1),
		secp256k1Curve.Point().Mul(secp256k1.IntToScalar(s), p2))
}

// Proof represents a proof that Gamma was constructed from the Seed
// according to the process mandated by the PublicKey.
//
// N.B.: The kyber.Point fields must contain secp256k1.secp256k1Point values
type Proof struct {
	PublicKey kyber.Point // secp256k1 public key of private key used in proof
	Gamma     kyber.Point
	C         *big.Int
	S         *big.Int
	Seed      *big.Int // Seed input to verifiable random function
	Output    *big.Int // verifiable random function output;, uniform uint256 sample
}

func (p *Proof) String() string {
	return fmt.Sprintf(
		"vrf.Proof{PublicKey: %s, Gamma: %s, C: %x, S: %x, Seed: %x, Output: %x}",
		p.PublicKey, p.Gamma, p.C, p.S, p.Seed, p.Output)
}

// WellFormed is true iff p's attributes satisfy basic domain checks
func (p *Proof) WellFormed() bool {
	return (secp256k1.ValidPublicKey(p.PublicKey) &&
		secp256k1.ValidPublicKey(p.Gamma) && secp256k1.RepresentsScalar(p.C) &&
		secp256k1.RepresentsScalar(p.S) && p.Output.BitLen() <= 256)
}

// checkCGammaNotEqualToSHash checks c*gamma ≠ s*hash, as required by solidity
// verifier
func checkCGammaNotEqualToSHash(c *big.Int, gamma kyber.Point, s *big.Int,
	hash kyber.Point) error {
	cGamma := secp256k1Curve.Point().Mul(secp256k1.IntToScalar(c), gamma)
	sHash := secp256k1Curve.Point().Mul(secp256k1.IntToScalar(s), hash)
	if cGamma.Equal(sHash) {
		return fmt.Errorf(
			"pick a different nonce; c*gamma = s*hash, with this one")
	}
	return nil
}

// VerifyProof is true iff gamma was generated in the mandated way from the
// given publicKey and seed, and no error was encountered
func (proof *Proof) Verify() (bool, error) {
	if !proof.WellFormed() {
		return false, fmt.Errorf("badly-formatted proof")
	}
	h, err := HashToCurve(proof.PublicKey, proof.Seed, func(*big.Int) {})
	if err != nil {
		return false, err
	}
	err = checkCGammaNotEqualToSHash(proof.C, proof.Gamma, proof.S, h)
	if err != nil {
		return false, fmt.Errorf("c*γ = s*hash (disallowed in solidity verifier)")
	}
	// publicKey = secretKey*Generator. See GenerateProof for u, v, m, s
	// c*secretKey*Generator + (m - c*secretKey)*Generator = m*Generator = u
	uPrime := linearCombination(proof.C, proof.PublicKey, proof.S, Generator)
	// c*secretKey*h + (m - c*secretKey)*h = m*h = v
	vPrime := linearCombination(proof.C, proof.Gamma, proof.S, h)
	uWitness, err := secp256k1.EthereumAddress(uPrime)
	if err != nil {
		return false, errors.Wrap(err, "vrf.VerifyProof#EthereumAddress")
	}
	cPrime := ScalarFromCurvePoints(h, proof.PublicKey, proof.Gamma, uWitness, vPrime)
	output, err := utils.Keccak256(secp256k1.LongMarshal(proof.Gamma))
	if err != nil {
		panic(errors.Wrap(err, "while hashing to compute proof output"))
	}
	return (proof.C.Cmp(cPrime) == 0) &&
			(proof.Output.Cmp(i().SetBytes(output)) == 0),
		nil
}

// generateProofWithNonce allows external nonce generation for testing purposes
func generateProofWithNonce(secretKey, seed, nonce *big.Int) (*Proof, error) {
	if !(secp256k1.RepresentsScalar(secretKey) && seed.BitLen() <= 256) {
		return nil, fmt.Errorf("badly-formatted key or seed")
	}
	publicKey := secp256k1Curve.Point().Mul(secp256k1.IntToScalar(secretKey), nil)
	h, err := HashToCurve(publicKey, seed, func(*big.Int) {})
	if err != nil {
		return &Proof{}, errors.Wrap(err, "vrf.makeProof#HashToCurve")
	}
	gamma := secp256k1Curve.Point().Mul(secp256k1.IntToScalar(secretKey), h)
	sm := secp256k1.IntToScalar(nonce)
	u := secp256k1Curve.Point().Mul(sm, Generator)
	uWitness, err := secp256k1.EthereumAddress(u)
	if err != nil {
		panic(errors.Wrap(err, "while computing Ethereum Address for proof"))
	}
	v := secp256k1Curve.Point().Mul(sm, h)
	c := ScalarFromCurvePoints(h, publicKey, gamma, uWitness, v)
	s := mod(sub(nonce, mul(c, secretKey)), Order) // (m - c*secretKey) % Order
	if err := checkCGammaNotEqualToSHash(c, gamma, s, h); err != nil {
		return nil, err
	}
	outputHash, err := utils.Keccak256(secp256k1.LongMarshal(gamma))
	if err != nil {
		panic("failed to hash gamma")
	}
	rv := Proof{
		PublicKey: publicKey,
		Gamma:     gamma,
		C:         c,
		S:         s,
		Seed:      seed,
		Output:    i().SetBytes(outputHash),
	}
	valid, err := rv.Verify()
	if !valid || err != nil {
		panic("constructed invalid proof")
	}
	return &rv, nil
}

// GenerateProof returns gamma, plus proof that gamma was constructed from seed
// as mandated from the given secretKey, with public key secretKey*Generator
//
// secretKey and seed must be less than secp256k1 group order. (Without this
// constraint on the seed, the samples and the possible public keys would
// deviate very slightly from uniform distribution.)
func GenerateProof(secretKey, seed *big.Int) (*Proof, error) {
	if secretKey.Cmp(zero) == -1 || seed.Cmp(zero) == -1 {
		return nil, fmt.Errorf("seed and/or secret key must be non-negative")
	}
	if secretKey.Cmp(Order) != -1 || seed.Cmp(Order) != -1 {
		return nil, fmt.Errorf("seed and/or secret key must be less than group order")
	}
	nonce, err := rand.Int(rand.Reader, Order)
	if err != nil {
		return nil, err
	}
	return generateProofWithNonce(secretKey, seed, nonce)
}
