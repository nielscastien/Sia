package host

import (
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"path/filepath"

	"github.com/NebulousLabs/Sia/build"
	"github.com/NebulousLabs/Sia/crypto"
	"github.com/NebulousLabs/Sia/modules"
	"github.com/NebulousLabs/Sia/persist"
	"github.com/NebulousLabs/Sia/types"

	"github.com/coreos/bbolt"
)

// persistence is the data that is kept when the host is restarted.
type persistence struct {
	// Consensus Tracking.
	BlockHeight  types.BlockHeight         `json:"blockheight"`
	RecentChange modules.ConsensusChangeID `json:"recentchange"`

	// Host Identity.
	Announced        bool                         `json:"announced"`
	AutoAddress      modules.NetAddress           `json:"autoaddress"`
	FinancialMetrics modules.HostFinancialMetrics `json:"financialmetrics"`
	PublicKey        types.SiaPublicKey           `json:"publickey"`
	RevisionNumber   uint64                       `json:"revisionnumber"`
	SecretKey        crypto.SecretKey             `json:"secretkey"`
	Settings         modules.HostInternalSettings `json:"settings"`
	UnlockHash       types.UnlockHash             `json:"unlockhash"`
}

// persistData returns the data in the Host that will be saved to disk.
func (h *Host) persistData() persistence {
	return persistence{
		// Consensus Tracking.
		BlockHeight:  h.blockHeight,
		RecentChange: h.recentChange,

		// Host Identity.
		Announced:        h.announced,
		AutoAddress:      h.autoAddress,
		FinancialMetrics: h.financialMetrics,
		PublicKey:        h.publicKey,
		RevisionNumber:   h.revisionNumber,
		SecretKey:        h.secretKey,
		Settings:         h.settings,
		UnlockHash:       h.unlockHash,
	}
}

// establishDefaults configures the default settings for the host, overwriting
// any existing settings.
func (h *Host) establishDefaults() error {
	// Configure the settings object.
	h.settings = modules.HostInternalSettings{
		MaxDownloadBatchSize: uint64(defaultMaxDownloadBatchSize),
		MaxDuration:          defaultMaxDuration,
		MaxReviseBatchSize:   uint64(defaultMaxReviseBatchSize),
		WindowSize:           defaultWindowSize,

		Collateral:       defaultCollateral,
		CollateralBudget: defaultCollateralBudget,
		MaxCollateral:    defaultMaxCollateral,

		MinStoragePrice:           defaultStoragePrice,
		MinContractPrice:          defaultContractPrice,
		MinDownloadBandwidthPrice: defaultDownloadBandwidthPrice,
		MinUploadBandwidthPrice:   defaultUploadBandwidthPrice,
	}

	// Generate signing key, for revising contracts.
	sk, pk := crypto.GenerateKeyPair()
	h.secretKey = sk
	h.publicKey = types.Ed25519PublicKey(pk)

	// Subscribe to the consensus set.
	err := h.initConsensusSubscription()
	if err != nil {
		return err
	}
	return nil
}

// loadPersistObject will take a persist object and copy the data into the
// host.
func (h *Host) loadPersistObject(p *persistence) {
	// Copy over consensus tracking.
	h.blockHeight = p.BlockHeight
	h.recentChange = p.RecentChange

	// Copy over host identity.
	h.announced = p.Announced
	h.autoAddress = p.AutoAddress
	if err := p.AutoAddress.IsValid(); err != nil {
		h.log.Printf("WARN: AutoAddress '%v' loaded from persist is invalid: %v", p.AutoAddress, err)
		h.autoAddress = ""
	}
	h.financialMetrics = p.FinancialMetrics
	h.publicKey = p.PublicKey
	h.revisionNumber = p.RevisionNumber
	h.secretKey = p.SecretKey
	h.settings = p.Settings
	if err := p.Settings.NetAddress.IsValid(); err != nil {
		h.log.Printf("WARN: NetAddress '%v' loaded from persist is invalid: %v", p.Settings.NetAddress, err)
		h.settings.NetAddress = ""
	}
	h.unlockHash = p.UnlockHash
}

// initDB will check that the database has been initialized and if not, will
// initialize the database.
func (h *Host) initDB() (err error) {
	// Open the host's database and set up the stop function to close it.
	h.db, err = h.dependencies.OpenDatabase(dbMetadata, filepath.Join(h.persistDir, dbFilename))
	if err != nil {
		return err
	}
	h.tg.AfterStop(func() {
		err = h.db.Close()
		if err != nil {
			h.log.Println("Could not close the database:", err)
		}
	})

	return h.db.Update(func(tx *bolt.Tx) error {
		// The storage obligation bucket does not exist, which means the
		// database needs to be initialized. Create the database buckets.
		buckets := [][]byte{
			bucketActionItems,
			bucketStorageObligations,
		}
		for _, bucket := range buckets {
			_, err := tx.CreateBucketIfNotExists(bucket)
			if err != nil {
				return err
			}
		}
		return nil
	})
}

// load loads the Hosts's persistent data from disk.
func (h *Host) load() error {
	// Initialize the host database.
	err := h.initDB()
	if err != nil {
		err = build.ExtendErr("Could not initialize database:", err)
		h.log.Println(err)
		return err
	}

	// Load the old persistence object from disk. Simple task if the version is
	// the most recent version, but older versions need to be updated to the
	// more recent structures.
	p := new(persistence)
	err = h.dependencies.LoadFile(persistMetadata, p, filepath.Join(h.persistDir, settingsFile))
	if err == nil {
		// Copy in the persistence.
		h.loadPersistObject(p)
	} else if os.IsNotExist(err) {
		// There is no host.json file, set up sane defaults.
		return h.establishDefaults()
	} else if err == persist.ErrBadVersion {
		// Attempt an upgrade from V112 to V120.
		err = h.upgradeFromV112ToV120()
		if err != nil {
			return err
		}
	} else if err != nil {
		return err
	}

	// Get the contract count by observing all of the incomplete storage
	// obligations in the database.
	h.financialMetrics.ContractCount = 0
	h.financialMetrics.LockedStorageCollateral = types.ZeroCurrency
	h.financialMetrics.PotentialStorageRevenue = types.ZeroCurrency
	h.financialMetrics.PotentialContractCompensation = types.ZeroCurrency
	h.financialMetrics.RiskedStorageCollateral = types.ZeroCurrency
	h.financialMetrics.TransactionFeeExpenses = types.ZeroCurrency
	err = h.db.View(func(tx *bolt.Tx) error {
		i := 0
		cursor := tx.Bucket(bucketStorageObligations).Cursor()
		for k, v := cursor.First(); k != nil; k, v = cursor.Next() {
			var so storageObligation
			err := json.Unmarshal(v, &so)
			if err != nil {
				return err
			}
			if so.ObligationStatus == obligationUnresolved {
				h.financialMetrics.ContractCount++
				h.financialMetrics.LockedStorageCollateral = h.financialMetrics.LockedStorageCollateral.Add(so.LockedCollateral)
				h.financialMetrics.PotentialStorageRevenue = h.financialMetrics.PotentialStorageRevenue.Add(so.PotentialStorageRevenue)
				h.financialMetrics.PotentialContractCompensation = h.financialMetrics.PotentialContractCompensation.Add(so.ContractCost)
				h.financialMetrics.RiskedStorageCollateral = h.financialMetrics.RiskedStorageCollateral.Add(so.RiskedCollateral)
			}
			if so.ObligationStatus == obligationRejected {
				i++
			}
			if so.ObligationStatus == obligationSucceeded || so.ObligationStatus == obligationFailed {
				h.financialMetrics.TransactionFeeExpenses = h.financialMetrics.TransactionFeeExpenses.Add(so.TransactionFeesAdded)
			}
		}
		h.log.Printf("Rejected SO's: %v", i)
		return nil
	})
	if err != nil {
		return err
	}

	return h.initConsensusSubscription()
}

// currencyUnits converts a types.Currency to a string with human-readable
// units. The unit used will be the largest unit that results in a value
// greater than 1. The value is rounded to 4 significant digits.
func currencyUnits(c types.Currency) string {
	pico := types.SiacoinPrecision.Div64(1e12)
	if c.Cmp(pico) < 0 {
		return c.String() + " H"
	}

	// iterate until we find a unit greater than c
	mag := pico
	unit := ""
	for _, unit = range []string{"pS", "nS", "uS", "mS", "SC", "KS", "MS", "GS", "TS"} {
		if c.Cmp(mag.Mul64(1e3)) < 0 {
			break
		} else if unit != "TS" {
			// don't want to perform this multiply on the last iter; that
			// would give us 1.235 TS instead of 1235 TS
			mag = mag.Mul64(1e3)
		}
	}

	num := new(big.Rat).SetInt(c.Big())
	denom := new(big.Rat).SetInt(mag.Big())
	res, _ := new(big.Rat).Mul(num, denom.Inv(denom)).Float64()

	return fmt.Sprintf("%.4g %s", res, unit)
}

// saveSync stores all of the persist data to disk and then syncs to disk.
func (h *Host) saveSync() error {
	return persist.SaveJSON(persistMetadata, h.persistData(), filepath.Join(h.persistDir, settingsFile))
}
