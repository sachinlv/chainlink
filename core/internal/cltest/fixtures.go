package cltest

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"testing"

	"github.com/smartcontractkit/chainlink/core/eth"
	"github.com/smartcontractkit/chainlink/core/store/models"

	gethCommon "github.com/ethereum/go-ethereum/common"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const ()

// MustHelloWorldAgreement returns a hello world agreement with the provided address added to the Oracle whitelist
func MustHelloWorldAgreement(t *testing.T, oracleAddress gethCommon.Address) string {
	template := MustReadFile(t, "testdata/hello_world_agreement.json")
	oracles := []string{oracleAddress.Hex()}
	sa, err := sjson.SetBytes(template, "oracles", oracles)
	if err != nil {
		t.Fatal(err)
	}
	return string(sa)

}

func CreateCredsFile(t *testing.T, user models.User) (string, func()) {
	credsFile, err := ioutil.TempFile(os.TempDir(), "apicredentials-")
	if err != nil {
		t.Fatal("Cannot create temporary file", err)
	}
	creds := []byte(fmt.Sprintf("%s\n%s", user.Email, Password))
	if _, err = credsFile.Write(creds); err != nil {
		t.Fatal("Failed to write to temporary file", err)
	}
	return credsFile.Name(), func() {
		os.Remove(credsFile.Name())
	}
}

// FixtureCreateJobViaWeb creates a job from a fixture using /v2/specs
func FixtureCreateJobViaWeb(t *testing.T, app *TestApplication, path string) models.JobSpec {
	return CreateSpecViaWeb(t, app, string(MustReadFile(t, path)))
}

// JSONFromFixture create models.JSON from file path
func JSONFromFixture(t *testing.T, path string) models.JSON {
	return JSONFromBytes(t, MustReadFile(t, path))
}

// JSONResultFromFixture create model.JSON with params.result found in the given file path
func JSONResultFromFixture(t *testing.T, path string) models.JSON {
	res := gjson.Get(string(MustReadFile(t, path)), "params.result")
	return JSONFromString(t, res.String())
}

// LogFromFixture create ethtypes.log from file path
func LogFromFixture(t *testing.T, path string) eth.Log {
	value := gjson.Get(string(MustReadFile(t, path)), "params.result")
	var el eth.Log
	require.NoError(t, json.Unmarshal([]byte(value.String()), &el))

	return el
}

// TxReceiptFromFixture create ethtypes.log from file path
func TxReceiptFromFixture(t *testing.T, path string) eth.TxReceipt {
	jsonStr := JSONFromFixture(t, path).Get("result").String()

	var receipt eth.TxReceipt
	err := json.Unmarshal([]byte(jsonStr), &receipt)
	require.NoError(t, err)

	return receipt
}
