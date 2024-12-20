package cpu

import (
	"fmt"
	"math"
	"time"

	"github.com/Layr-Labs/eigenda/encoding/fft"
	"github.com/Layr-Labs/eigenda/encoding/kzg"
	"github.com/Layr-Labs/eigenda/encoding/utils/toeplitz"
	"github.com/consensys/gnark-crypto/ecc"
	"github.com/consensys/gnark-crypto/ecc/bn254"
	"github.com/consensys/gnark-crypto/ecc/bn254/fr"
)

type KzgCpuProofDevice struct {
	*kzg.KzgConfig
	Fs         *fft.FFTSettings
	FFTPointsT [][]bn254.G1Affine // transpose of FFTPoints
	SFs        *fft.FFTSettings
	Srs        *kzg.SRS
	G2Trailing []bn254.G2Affine
}

type WorkerResult struct {
	err error
}

func (p *KzgCpuProofDevice) ComputeLengthProof(coeffs []fr.Element) (*bn254.G2Affine, error) {
	inputLength := uint64(len(coeffs))
	return p.ComputeLengthProofForLength(coeffs, inputLength)
}

func (p *KzgCpuProofDevice) ComputeLengthProofForLength(coeffs []fr.Element, length uint64) (*bn254.G2Affine, error) {

	if length < uint64(len(coeffs)) {
		return nil, fmt.Errorf("length is less than the number of coefficients")
	}

	start := p.KzgConfig.SRSNumberToLoad - length
	shiftedSecret := p.G2Trailing[start : start+uint64(len(coeffs))]
	config := ecc.MultiExpConfig{}
	//The proof of low degree is commitment of the polynomial shifted to the largest srs degree
	var lengthProof bn254.G2Affine
	_, err := lengthProof.MultiExp(shiftedSecret, coeffs, config)
	if err != nil {
		return nil, err
	}
	return &lengthProof, nil

}

func (p *KzgCpuProofDevice) ComputeCommitment(coeffs []fr.Element) (*bn254.G1Affine, error) {
	// compute commit for the full poly
	config := ecc.MultiExpConfig{}
	var commitment bn254.G1Affine
	_, err := commitment.MultiExp(p.Srs.G1[:len(coeffs)], coeffs, config)
	if err != nil {
		return nil, err
	}
	return &commitment, nil
}

func (p *KzgCpuProofDevice) ComputeLengthCommitment(coeffs []fr.Element) (*bn254.G2Affine, error) {
	config := ecc.MultiExpConfig{}

	var lengthCommitment bn254.G2Affine
	_, err := lengthCommitment.MultiExp(p.Srs.G2[:len(coeffs)], coeffs, config)
	if err != nil {
		return nil, err
	}
	return &lengthCommitment, nil
}

func (p *KzgCpuProofDevice) ComputeMultiFrameProof(polyFr []fr.Element, numChunks, chunkLen, numWorker uint64) ([]bn254.G1Affine, error) {
	begin := time.Now()
	// Robert: Standardizing this to use the same math used in precomputeSRS
	dimE := numChunks
	l := chunkLen

	sumVec := make([]bn254.G1Affine, dimE*2)

	jobChan := make(chan uint64, numWorker)
	results := make(chan WorkerResult, numWorker)

	// create storage for intermediate fft outputs
	coeffStore := make([][]fr.Element, dimE*2)
	for i := range coeffStore {
		coeffStore[i] = make([]fr.Element, l)
	}

	for w := uint64(0); w < numWorker; w++ {
		go p.proofWorker(polyFr, jobChan, l, dimE, coeffStore, results)
	}

	for j := uint64(0); j < l; j++ {
		jobChan <- j
	}
	close(jobChan)

	// return last error
	var err error
	for w := uint64(0); w < numWorker; w++ {
		wr := <-results
		if wr.err != nil {
			err = wr.err
		}
	}

	if err != nil {
		return nil, fmt.Errorf("proof worker error: %v", err)
	}

	preprocessDone := time.Now()

	// compute proof by multi scaler multiplication
	msmErrors := make(chan error, dimE*2)
	for i := uint64(0); i < dimE*2; i++ {

		go func(k uint64) {
			_, err := sumVec[k].MultiExp(p.FFTPointsT[k], coeffStore[k], ecc.MultiExpConfig{})
			// handle error
			msmErrors <- err
		}(i)
	}

	for i := uint64(0); i < dimE*2; i++ {
		err := <-msmErrors
		if err != nil {
			fmt.Println("Error. MSM while adding points", err)
			return nil, err
		}
	}

	msmDone := time.Now()

	// only 1 ifft is needed
	sumVecInv, err := p.Fs.FFTG1(sumVec, true)
	if err != nil {
		return nil, fmt.Errorf("fft error: %v", err)
	}

	firstECNttDone := time.Now()

	// outputs is out of order - buttefly
	proofs, err := p.Fs.FFTG1(sumVecInv[:dimE], false)
	if err != nil {
		return nil, err
	}

	secondECNttDone := time.Now()

	fmt.Printf("Multiproof Time Decomp \n\t\ttotal   %-20s \n\t\tpreproc %-20s \n\t\tmsm     %-20s \n\t\tfft1    %-20s \n\t\tfft2    %-20s\n",
		secondECNttDone.Sub(begin).String(),
		preprocessDone.Sub(begin).String(),
		msmDone.Sub(preprocessDone).String(),
		firstECNttDone.Sub(msmDone).String(),
		secondECNttDone.Sub(firstECNttDone).String(),
	)

	return proofs, nil
}

func (p *KzgCpuProofDevice) proofWorker(
	polyFr []fr.Element,
	jobChan <-chan uint64,
	l uint64,
	dimE uint64,
	coeffStore [][]fr.Element,
	results chan<- WorkerResult,
) {

	for j := range jobChan {
		coeffs, err := p.GetSlicesCoeff(polyFr, dimE, j, l)
		if err != nil {
			results <- WorkerResult{
				err: err,
			}
		} else {
			for i := 0; i < len(coeffs); i++ {
				coeffStore[i][j] = coeffs[i]
			}
		}
	}

	results <- WorkerResult{
		err: nil,
	}
}

// output is in the form see primeField toeplitz
//
// phi ^ (coset size ) = 1
//
// implicitly pad slices to power of 2
func (p *KzgCpuProofDevice) GetSlicesCoeff(polyFr []fr.Element, dimE, j, l uint64) ([]fr.Element, error) {
	// there is a constant term
	m := uint64(len(polyFr)) - 1
	dim := (m - j) / l

	// maximal number of unique values from a toeplitz matrix
	tDim := 2*dimE - 1

	toeV := make([]fr.Element, tDim)
	for i := uint64(0); i < dim; i++ {

		toeV[i].Set(&polyFr[m-(j+i*l)])
	}

	// use precompute table
	tm, err := toeplitz.NewToeplitz(toeV, p.SFs)
	if err != nil {
		return nil, err
	}
	return tm.GetFFTCoeff()
}

/*
returns the power of 2 which is immediately bigger than the input
*/
func CeilIntPowerOf2Num(d uint64) uint64 {
	nextPower := math.Ceil(math.Log2(float64(d)))
	return uint64(math.Pow(2.0, nextPower))
}
