/* This Source Code Form is subject to the terms of the Mozilla Public
 * License, v. 2.0. If a copy of the MPL was not distributed with this
 * file, You can obtain one at http://mozilla.org/MPL/2.0/. */

// Implements the CT Log as a SQL Database

package sqldb

import (
	"encoding/base64"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/go-gorp/gorp"
	"github.com/google/certificate-transparency/go"
	"github.com/google/certificate-transparency/go/x509"
	"github.com/jcjones/ct-sql/censysdata"
	"golang.org/x/net/publicsuffix"
)

type Certificate struct {
	CertID    uint64    `db:"certID"`    // Internal Cert Identifier
	Serial    string    `db:"serial"`    // The serial number of this cert
	IssuerID  int       `db:"issuerID"`  // The Issuer of this cert
	Subject   string    `db:"subject"`   // The Subject field of this cert
	NotBefore time.Time `db:"notBefore"` // Date before which this cert should be considered invalid
	NotAfter  time.Time `db:"notAfter"`  // Date after which this cert should be considered invalid
}

type Issuer struct {
	IssuerID       int    `db:"issuerID"`       // Internal Issuer ID
	CommonName     string `db:"commonName"`     // Issuer CN
	AuthorityKeyId string `db:"authorityKeyId"` // Authority Key ID
}

type SubjectName struct {
	CertID uint64 `db:"certID"` // Internal Cert Identifier
	Name   string `db:"name"`   // identifier
}

type RegisteredDomain struct {
	CertID uint64 `db:"certID"` // Internal Cert Identifier
	ETLD   string `db:"etld"`   // effective top-level domain
	Label  string `db:"label"`  // first label
	Domain string `db:"domain"` // eTLD+first label
}

type CertificateRaw struct {
  CertID uint64 `db:"certID"` // Internal Cert Identifier
  DER   []byte  `db:"der"`    // DER-encoded certificate
}

type CertificateLog struct {
	LogID int    `db:"logId"` // Log Identifier (FK to CertificateLog)
	URL   string `db:"url"`   // URL to the log
}

type CertificateLogEntry struct {
	CertID    uint64    `db:"certID"`    // Internal Cert Identifier (FK to Certificate)
	LogID     int       `db:"logId"`     // Log Identifier (FK to CertificateLog)
	EntryID   uint64    `db:"entryId"`   // Entry Identifier within the log
	EntryTime time.Time `db:"entryTime"` // Date when this certificate was added to the log
}

type CensysEntry struct {
	CertID    uint64    `db:"certID"`    // Internal Cert Identifier (FK to Certificate)
	EntryTime time.Time `db:"entryTime"` // Date when this certificate was imported from Censys.io
}

func Uint64ToTimestamp(timestamp uint64) time.Time {
	return time.Unix(int64(timestamp/1000), int64(timestamp%1000))
}

type EntriesDatabase struct {
	DbMap        *gorp.DbMap
	LogId        int
	Verbose      bool
  FullCerts    bool
	IssuerFilter *string
}

func (edb *EntriesDatabase) InitTables() error {
	if edb.Verbose {
		edb.DbMap.TraceOn("[gorp]", log.New(os.Stdout, "myapp:", log.Lmicroseconds))
	}

  certRawTable := edb.DbMap.AddTableWithName(CertificateRaw{}, "certificateraw")
  certRawTable.SetKeys(true, "CertID")

	domainTable := edb.DbMap.AddTableWithName(RegisteredDomain{}, "registereddomain")
	domainTable.AddIndex("CertIDIdx", "BTree", []string{"CertID"})
	domainTable.AddIndex("DomainIdx", "Hash", []string{"Domain"})
	domainTable.AddIndex("LabelIdx", "Hash", []string{"Label"})
	domainTable.SetUniqueTogether("CertID", "Domain")

	censysEntryTable := edb.DbMap.AddTableWithName(CensysEntry{}, "censysentry")
	censysEntryTable.AddIndex("CertIDIdx", "BTree", []string{"CertID"})

	logTable := edb.DbMap.AddTableWithName(CertificateLog{}, "ctlog")
	logTable.SetKeys(true, "LogID")
	logTable.ColMap("URL").SetUnique(true)

	certTable := edb.DbMap.AddTableWithName(Certificate{}, "certificate")
	certTable.SetKeys(true, "CertID")
	certTable.AddIndex("SerialIdx", "Hash", []string{"Serial"})
	certTable.AddIndex("notBeforeIdx", "Hash", []string{"NotBefore"})
	certTable.AddIndex("notAfterIdx", "Hash", []string{"NotAfter"})
	certTable.SetUniqueTogether("Serial", "IssuerID")

	issuerTable := edb.DbMap.AddTableWithName(Issuer{}, "issuer")
	issuerTable.SetKeys(true, "IssuerID")
	issuerTable.ColMap("AuthorityKeyId").SetUnique(true)
	issuerTable.AddIndex("CNIdx", "Hash", []string{"CommonName"})
	issuerTable.AddIndex("AKIIdx", "Hash", []string{"AuthorityKeyId"})

	logEntryTable := edb.DbMap.AddTableWithName(CertificateLogEntry{}, "ctlogentry")
	logEntryTable.AddIndex("CertIDIdx", "BTree", []string{"CertID"})
	logEntryTable.SetUniqueTogether("LogID", "EntryID")

	nameTable := edb.DbMap.AddTableWithName(SubjectName{}, "name")
	nameTable.AddIndex("CertIDIdx", "BTree", []string{"CertID"})
	nameTable.AddIndex("NameIdx", "Hash", []string{"Name"})
	nameTable.SetUniqueTogether("CertID", "Name")

	err := edb.DbMap.CreateTablesIfNotExists()
	if err != nil && edb.Verbose {
		fmt.Println(fmt.Sprintf("Note: could not create tables %s", err))
	}

	err = edb.DbMap.CreateIndex()
	if err != nil && edb.Verbose {
		fmt.Println(fmt.Sprintf("Note: could not create indicies %s", err))
	}

	// All is well, no matter what.
	return nil
}

func (edb *EntriesDatabase) Count() (count uint64, err error) {
	err = edb.DbMap.SelectOne(&count, "SELECT CASE WHEN MAX(e.entryId) IS NULL THEN 0 ELSE MAX(e.entryId)+1 END FROM ctlogentry AS e WHERE e.logId = ?", edb.LogId)
	return
}

func (edb *EntriesDatabase) SetLog(url string) error {
	var certLogObj CertificateLog

	err := edb.DbMap.SelectOne(&certLogObj, "SELECT * FROM ctlog WHERE url = ?", url)
	if err != nil {
		// Couldn't find it. Set the object and insert it.
		certLogObj.URL = url

		err = edb.DbMap.Insert(&certLogObj)
		if err != nil {
			return err
		}
	}

	edb.LogId = certLogObj.LogID
	return nil
}

func (edb *EntriesDatabase) GetNamesWithoutRegisteredDomains(limit uint64) ([]uint64, error) {
	var results []uint64
	query := "SELECT DISTINCT certID FROM name NATURAL LEFT JOIN registereddomain WHERE etld IS NULL"

	if limit > 0 {
		_, err := edb.DbMap.Select(&results, query+" LIMIT ?", limit)
		return results, err
	}

	_, err := edb.DbMap.Select(&results, query)
	return results, err
}

func (edb *EntriesDatabase) ReprocessRegisteredDomainsForCertId(certID uint64) error {
	txn, err := edb.DbMap.Begin()
	if err != nil {
		return err
	}

	results := []SubjectName{}
	_, err = edb.DbMap.Select(&results, "SELECT * FROM name WHERE certID = ?", certID)

	nameMap := make(map[string]struct{})
	for _, nameObj := range results {
		if nameObj.CertID != certID {
			return fmt.Errorf("CertID didn't match! expected=%d obj=%+v", certID, nameObj)
		}
		nameMap[nameObj.Name] = struct{}{}
	}

	err = edb.insertRegisteredDomains(txn, certID, nameMap)
	if err != nil {
		txn.Rollback()
		return err
	}

	return txn.Commit()
}

func (edb *EntriesDatabase) insertCertificate(cert *x509.Certificate) (*gorp.Transaction, uint64, error) {
	//
	// Find the Certificate's issuing CA, using a loop since this is contentious.
	// Also, this is lame. TODO: Be smarter with insertion mutexes
	//

	var issuerObj Issuer
	for {
		// Try to find a matching one first
		err := edb.DbMap.SelectOne(&issuerObj, "SELECT * FROM issuer WHERE authorityKeyId = ?", base64.StdEncoding.EncodeToString(cert.AuthorityKeyId))
		if err != nil {
			//
			// This is a new issuer, so let's add it to the database
			//
			issuerObj := &Issuer{
				AuthorityKeyId: base64.StdEncoding.EncodeToString(cert.AuthorityKeyId),
				CommonName:     cert.Issuer.CommonName,
			}
			err = edb.DbMap.Insert(issuerObj)
			if err == nil {
				// It worked! Proceed.
				break
			}
			log.Printf("Collision on issuer %v, retrying", issuerObj)
		} else {
			break
		}
	}

	//
	// Now that Issuer (which is contentious) is resolved / committed,
	// start a transaction
	//

	txn, err := edb.DbMap.Begin()
	if err != nil {
		return nil, 0, err
	}

	// Parse the serial number
	serialNum := fmt.Sprintf("%036x", cert.SerialNumber)

	//
	// Find/insert the Certificate from/into the DB.
	//

	var certId uint64

	err = txn.SelectOne(&certId, "SELECT certID FROM certificate WHERE serial = ? AND issuerID = ?", serialNum, issuerObj.IssuerID)
	if err != nil {
		//
		// This is a new certificate, so we need to add it to the certificate DB
		// as well as pull out its metadata
		//

		if edb.Verbose {
			fmt.Println(fmt.Sprintf("Processing %s %#v", serialNum, cert.Subject.CommonName))
		}

		certObj := &Certificate{
			Serial:    serialNum,
			IssuerID:  issuerObj.IssuerID,
			Subject:   cert.Subject.CommonName,
			NotBefore: cert.NotBefore.UTC(),
			NotAfter:  cert.NotAfter.UTC(),
		}
		err = txn.Insert(certObj)
		if err != nil {
      // Don't die on insertion errors; they're likely duplicates, which
      // can happen, but we'll just ignore and make sure the names are valid.
      log.Printf("DB error on cert: %#v: %s", certObj, err)
		}

		certId = certObj.CertID

		//
		// Process the DNS Names in the Certificate
		//

		// De-dupe the CN and the SAN
		names := make(map[string]struct{})
		if cert.Subject.CommonName != "" {
			names[cert.Subject.CommonName] = struct{}{}
		}
		for _, name := range cert.DNSNames {
			names[name] = struct{}{}
		}

		// Loop and insert names into the DB
		for name, _ := range names {
			nameObj := &SubjectName{
				Name:   name,
				CertID: certId,
			}
			// Ignore errors on insert
			_ = txn.Insert(nameObj)
		}

		err = edb.insertRegisteredDomains(txn, certId, names)
		if err != nil {
      log.Printf("DB error on registered domains: %#v: %s", certObj, err)
		}
	}

  //
  // Insert the raw certificate, if not already there
  //
  if edb.FullCerts {
    rawCert, err := txn.Get(&CertificateRaw{}, certId)
    if err != nil {
      log.Printf("DB error on raw certificate: %d: %s", certId, err)
    }
    if rawCert == nil {
      rawCertObj := &CertificateRaw{
        CertID: certId,
        DER: cert.Raw,
      }
      // Ignore errors on insert
      _ = txn.Insert(rawCertObj)
    }
  }

	return txn, certId, err
}

func (edb *EntriesDatabase) insertRegisteredDomains(txn *gorp.Transaction, certId uint64, names map[string]struct{}) error {
	domains := make(map[string]struct{})
	for name, _ := range names {
		domain, err := publicsuffix.EffectiveTLDPlusOne(name)
		if err != nil {
			// This is non-critical. We'd rather have the cert with an incomplete
			// eTLD, so mask this error
			if edb.Verbose {
				fmt.Printf("%s\n", err)
			}
			continue
		}
		domains[domain] = struct{}{}
	}
	for domain, _ := range domains {
		etld, _ := publicsuffix.PublicSuffix(domain)
		label := strings.Replace(domain, "."+etld, "", 1)
		domainObj := &RegisteredDomain{
			CertID: certId,
			Domain: domain,
			ETLD:   etld,
			Label:  label,
		}
		// Ignore errors on insert
		_ = txn.Insert(domainObj)
	}
	return nil
}

func (edb *EntriesDatabase) InsertCensysEntry(entry *censysdata.CensysEntry) error {
	cert, err := x509.ParseCertificate(entry.CertBytes)
	if err != nil {
		return err
	}

	txn, certId, err := edb.insertCertificate(cert)
	if err != nil {
		return err
	}

	//
	// Insert the appropriate CertificateLogEntry
	//
	var entryCount int

	err = txn.SelectOne(&entryCount, "SELECT COUNT(1) FROM censysentry WHERE certID = ?", certId)
	if err != nil {
		txn.Rollback()
		return fmt.Errorf("DB error finding existing censys entries for CertID %v: %s", certId, err)
	}

	if entryCount == 0 {
		// Not found yet, so insert it.
		certEntry := &CensysEntry{
			CertID:    certId,
			EntryTime: *entry.Timestamp,
		}
		err = txn.Insert(certEntry)
		if err != nil {
			txn.Rollback()
			return fmt.Errorf("DB error on censys entry: %#v: %s", certEntry, err)
		}
	}
	err = txn.Commit()
	return err
}

func (edb *EntriesDatabase) InsertCTEntry(entry *ct.LogEntry) error {
	var cert *x509.Certificate
	var err error

	switch entry.Leaf.TimestampedEntry.EntryType {
	case ct.X509LogEntryType:
		cert, err = x509.ParseCertificate(entry.Leaf.TimestampedEntry.X509Entry)
	case ct.PrecertLogEntryType:
		cert, err = x509.ParseTBSCertificate(entry.Leaf.TimestampedEntry.PrecertEntry.TBSCertificate)
	}

	if err != nil {
		return err
	}

	// Skip unimportant entries, if configured
	if edb.IssuerFilter != nil && !strings.HasPrefix(cert.Issuer.CommonName, *edb.IssuerFilter) {
		return nil
	}

	txn, certId, err := edb.insertCertificate(cert)
	if err != nil {
		return err
	}

	//
	// Insert the appropriate CertificateLogEntry
	//
	var entryCount int

	err = txn.SelectOne(&entryCount, "SELECT COUNT(1) FROM ctlogentry WHERE entryID = ? AND logID = ?", entry.Index, edb.LogId)
	if err != nil {
		txn.Rollback()
		return fmt.Errorf("DB error finding existing log entries for CertID %v with LogID %v: %s", certId, edb.LogId, err)
	}

	if entryCount == 0 {
		// Not found yet, so insert it.
		certLogEntry := &CertificateLogEntry{
			CertID:    certId,
			LogID:     edb.LogId,
			EntryID:   uint64(entry.Index),
			EntryTime: Uint64ToTimestamp(entry.Leaf.TimestampedEntry.Timestamp),
		}
		err = txn.Insert(certLogEntry)
		if err != nil {
			txn.Rollback()
			return fmt.Errorf("DB error on cert log entry: %#v: %s", certLogEntry, err)
		}
	}

	err = txn.Commit()
	return err
}
