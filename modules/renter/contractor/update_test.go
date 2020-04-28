package contractor

import (
	"fmt"
	"testing"
	"time"

	"gitlab.com/NebulousLabs/errors"
	"gitlab.com/NebulousLabs/fastrand"

	"gitlab.com/NebulousLabs/Sia/build"
	"gitlab.com/NebulousLabs/Sia/crypto"
	"gitlab.com/NebulousLabs/Sia/modules"
	"gitlab.com/NebulousLabs/Sia/types"
)

// TestIntegrationAutoRenew tests that contracts are automatically renewed at
// the expected block height.
func TestIntegrationAutoRenew(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
	}
	t.Parallel()
	// create testing trio
	_, c, m, err := newTestingTrio(t.Name())
	if err != nil {
		t.Fatal(err)
	}

	// form a contract with the host
	a := modules.Allowance{
		Funds:              types.SiacoinPrecision.Mul64(100), // 100 SC
		Hosts:              1,
		Period:             50,
		RenewWindow:        10,
		ExpectedStorage:    modules.DefaultAllowance.ExpectedStorage,
		ExpectedUpload:     modules.DefaultAllowance.ExpectedUpload,
		ExpectedDownload:   modules.DefaultAllowance.ExpectedDownload,
		ExpectedRedundancy: modules.DefaultAllowance.ExpectedRedundancy,
		MaxPeriodChurn:     modules.DefaultAllowance.MaxPeriodChurn,
	}
	err = c.SetAllowance(a)
	if err != nil {
		t.Fatal(err)
	}
	numRetries := 0
	err = build.Retry(100, 100*time.Millisecond, func() error {
		if numRetries%10 == 0 {
			if _, err := m.AddBlock(); err != nil {
				return err
			}
		}
		numRetries++
		if len(c.Contracts()) == 0 {
			return errors.New("contracts were not formed")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	contract := c.Contracts()[0]

	// revise the contract
	editor, err := c.Editor(contract.HostPublicKey, nil)
	if err != nil {
		t.Fatal(err)
	}
	data := fastrand.Bytes(int(modules.SectorSize))
	// insert the sector
	_, err = editor.Upload(data)
	if err != nil {
		t.Fatal(err)
	}
	err = editor.Close()
	if err != nil {
		t.Fatal(err)
	}

	// mine until we enter the renew window
	renewHeight := contract.EndHeight - c.allowance.RenewWindow
	for c.blockHeight < renewHeight {
		_, err := m.AddBlock()
		if err != nil {
			t.Fatal(err)
		}
	}
	// wait for goroutine in ProcessConsensusChange to finish
	time.Sleep(100 * time.Millisecond)
	c.maintenanceLock.Lock()
	c.maintenanceLock.Unlock()

	// check renewed contract
	contract = c.Contracts()[0]
	endHeight := c.contractEndHeight()
	if contract.EndHeight != endHeight {
		t.Fatalf("Wrong end height, expected %v got %v\n", endHeight, contract.EndHeight)
	}
}

// TestIntegrationRenewInvalidate tests that editors and downloaders are
// properly invalidated when a renew is queued.
func TestIntegrationRenewInvalidate(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
	}
	t.Parallel()
	// create testing trio
	_, c, m, err := newTestingTrio(t.Name())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	defer m.Close()

	// form a contract with the host
	a := modules.Allowance{
		Funds:              types.SiacoinPrecision.Mul64(100), // 100 SC
		Hosts:              1,
		Period:             50,
		RenewWindow:        10,
		ExpectedStorage:    modules.DefaultAllowance.ExpectedStorage,
		ExpectedUpload:     modules.DefaultAllowance.ExpectedUpload,
		ExpectedDownload:   modules.DefaultAllowance.ExpectedDownload,
		ExpectedRedundancy: modules.DefaultAllowance.ExpectedRedundancy,
		MaxPeriodChurn:     modules.DefaultAllowance.MaxPeriodChurn,
	}
	err = c.SetAllowance(a)
	if err != nil {
		t.Fatal(err)
	}
	numRetries := 0
	err = build.Retry(100, 100*time.Millisecond, func() error {
		if numRetries%10 == 0 {
			if _, err := m.AddBlock(); err != nil {
				return err
			}
		}
		numRetries++
		// Check for number of contracts and number of pubKeys as there is a
		// slight delay between the contract being added to the contract set and
		// the pubkey being added to the contractor map
		c.mu.Lock()
		numPubKeys := len(c.pubKeysToContractID)
		c.mu.Unlock()
		numContracts := len(c.Contracts())
		if numContracts != 1 {
			return fmt.Errorf("Expected 1 contracts, found %v", numContracts)
		}
		if numPubKeys != 1 {
			return fmt.Errorf("Expected 1 pubkey, found %v", numPubKeys)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	contract := c.Contracts()[0]

	// revise the contract
	editor, err := c.Editor(contract.HostPublicKey, nil)
	if err != nil {
		t.Fatal(err)
	}
	data := fastrand.Bytes(int(modules.SectorSize))
	// insert the sector
	_, err = editor.Upload(data)
	if err != nil {
		t.Fatal(err)
	}

	// mine until we enter the renew window; the editor should be invalidated
	renewHeight := contract.EndHeight - c.allowance.RenewWindow
	for c.blockHeight < renewHeight {
		_, err := m.AddBlock()
		if err != nil {
			t.Fatal(err)
		}
	}
	// wait for goroutine in ProcessConsensusChange to finish
	time.Sleep(100 * time.Millisecond)
	c.maintenanceLock.Lock()
	c.maintenanceLock.Unlock()

	// check renewed contract
	contract = c.Contracts()[0]
	endHeight := c.contractEndHeight()
	c.mu.Lock()
	if contract.EndHeight != endHeight {
		t.Fatalf("Wrong end height, expected %v got %v\n", endHeight, contract.EndHeight)
	}
	c.mu.Unlock()

	// editor should have been invalidated
	_, err = editor.Upload(make([]byte, modules.SectorSize))
	if err != errInvalidEditor && err != errInvalidSession {
		t.Error("expected invalid editor error; got", err)
	}
	editor.Close()

	// create a downloader
	downloader, err := c.Downloader(contract.HostPublicKey, nil)
	if err != nil {
		t.Fatal(err)
	}
	// mine until we enter the renew window
	renewHeight = contract.EndHeight - c.allowance.RenewWindow
	for c.blockHeight < renewHeight {
		_, err := m.AddBlock()
		if err != nil {
			t.Fatal(err)
		}
	}

	// downloader should have been invalidated
	err = build.Retry(50, 100*time.Millisecond, func() error {
		// wait for goroutine in ProcessConsensusChange to finish
		c.maintenanceLock.Lock()
		c.maintenanceLock.Unlock()
		_, err2 := downloader.Download(crypto.Hash{}, 0, 0)
		if err2 != errInvalidDownloader && err2 != errInvalidSession {
			return errors.AddContext(err, "expected invalid downloader error")
		}
		return downloader.Close()
	})
	if err != nil {
		t.Fatal(err)
	}
}
