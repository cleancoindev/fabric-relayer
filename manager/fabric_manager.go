/*
* Copyright (C) 2020 The poly network Authors
* This file is part of The poly network library.
*
* The poly network is free software: you can redistribute it and/or modify
* it under the terms of the GNU Lesser General Public License as published by
* the Free Software Foundation, either version 3 of the License, or
* (at your option) any later version.
*
* The poly network is distributed in the hope that it will be useful,
* but WITHOUT ANY WARRANTY; without even the implied warranty of
* MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
* GNU Lesser General Public License for more details.
* You should have received a copy of the GNU Lesser General Public License
* along with The poly network . If not, see <http://www.gnu.org/licenses/>.
 */
package manager

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/pem"
	"github.com/polynetwork/fabric-relayer/config"
	"github.com/polynetwork/fabric-relayer/db"
	"github.com/polynetwork/fabric-relayer/log"
	"github.com/polynetwork/fabric-relayer/tools"
	sdk "github.com/polynetwork/poly-go-sdk"
	"github.com/polynetwork/poly/common"
	"github.com/polynetwork/poly/native/service/cross_chain_manager/fabric"
	scom "github.com/polynetwork/poly/native/service/header_sync/common"
	autils "github.com/polynetwork/poly/native/service/utils"
	"github.com/tjfoc/gmsm/pkcs12"
	"github.com/tjfoc/gmsm/sm2"
	"io/ioutil"
	"time"
)

type FabricManager struct {
	config        *config.ServiceConfig
	client        *tools.FabricSdk
	polySdk       *sdk.PolySdk
	polySigner    *sdk.Account
	exitChan      chan int
	db            *db.BoltDB
	currentHeight uint64
	fabPrivks []*ecdsa.PrivateKey
	multiTrustChain scom.MultiCertTrustChain
}

func NewFabricManager(
	servconfig *config.ServiceConfig,
	ontsdk *sdk.PolySdk,
	client *tools.FabricSdk,
	boltDB *db.BoltDB,
) (mgr *FabricManager, err error) {

	var (
		wallet *sdk.Wallet
		signer *sdk.Account

		wf  = servconfig.PolyConfig.WalletFile
		pwd = []byte(servconfig.PolyConfig.WalletPwd)
	)

	// open wallet with poly sdk
	if !common.FileExisted(wf) {
		wallet, err = ontsdk.CreateWallet(wf)
	} else {
		wallet, err = ontsdk.OpenWallet(wf)
	}
	if err != nil {
		log.Errorf("NewFabricManager - open poly wallet error: %s", err.Error())
		return
	}

	// get account
	if signer, err = wallet.GetDefaultAccount(pwd); err != nil || signer == nil {
		signer, err = wallet.NewDefaultSettingAccount(pwd)
	}
	if err != nil {
		log.Errorf("NewFabricManager - poly wallet password error")
		return nil, err
	}

	// save wallet
	if err = wallet.Save(); err != nil {
		return
	}

	mtc := scom.MultiCertTrustChain(make([]*scom.CertTrustChain, len(servconfig.FabricConfig.TrustChainFiles)))
	for i, files := range servconfig.FabricConfig.TrustChainFiles {
		tc := &scom.CertTrustChain{
			Certs: make([]*sm2.Certificate, len(files)),
		}
		for j, tcFile := range files {
			raw, err := ioutil.ReadFile(tcFile)
			if err != nil {
				log.Errorf("NewFabricManager - failed to read %s: %v", tcFile, err)
				return nil, err
			}

			blk, _ := pem.Decode(raw)
			tc.Certs[j], err = sm2.ParseCertificate(blk.Bytes)
			if err != nil {
				log.Errorf("NewFabricManager - failed to parse %s to cert: %v", tcFile, err)
				return nil, err
			}
		}
		mtc[i] = tc
	}

	privks := make([]*ecdsa.PrivateKey, len(servconfig.FabricConfig.PrivateKeyFiles))
	for i, file := range servconfig.FabricConfig.PrivateKeyFiles {
		raw, err := ioutil.ReadFile(file)
		if err != nil {
			log.Errorf("NewFabricManager - failed to read %s: %v", file, err)
			return nil, err
		}
		blk, _ := pem.Decode(raw)
		key, err := pkcs12.ParsePKCS8PrivateKey(blk.Bytes)
		if err != nil {
			log.Errorf("NewFabricManager - failed to parse %s to private key: %v", file, err)
			return nil, err
		}
		privks[i] = key.(*ecdsa.PrivateKey)
	}

	log.Infof("NewFabricManager - poly user address: %s", signer.Address.ToBase58())

	mgr = &FabricManager{
		config:     servconfig,
		exitChan:   make(chan int),
		client:     client,
		polySdk:    ontsdk,
		polySigner: signer,
		db:         boltDB,
		multiTrustChain: mtc,
		fabPrivks: privks,
	}
	return mgr, nil
}

func (this *FabricManager) init() error {
	// get latest height
	latestHeight := this.findLastestHeight()
	log.Infof("init - latest synced height: %d", latestHeight)
	this.currentHeight = latestHeight
	return nil
}

func (this *FabricManager) findLastestHeight() uint64 {
	// try to get key
	var sideChainId uint64 = this.config.FabricConfig.SideChainId
	var sideChainIdBytes [8]byte
	binary.LittleEndian.PutUint64(sideChainIdBytes[:], sideChainId)
	contractAddress := autils.HeaderSyncContractAddress
	key := append([]byte(scom.CURRENT_HEADER_HEIGHT), sideChainIdBytes[:]...)
	// try to get storage
	result, err := this.polySdk.GetStorage(contractAddress.ToHexString(), key)
	if err != nil {
		log.Errorf("find latest height err: %v", err)
		return 3
	}
	if result == nil || len(result) == 0 {
		return 3
	} else {
		return binary.LittleEndian.Uint64(result)
	}
}

func (e *FabricManager) MonitorChain() {
	err := e.init()
	if err != nil {
		log.Errorf("init failed! err: %v", err)
		return
	}
	monitorTicker := time.NewTicker(config.FABRIC_MONITOR_INTERVAL)
	for {
		select {
		case <-monitorTicker.C:
			height, err := e.client.GetLatestHeight()
			if err != nil {
				log.Errorf("MonitorChain - cannot get node height, err: %s", err)
				continue
			}
			if height - e.currentHeight <= e.config.FabricConfig.BlockConfig {
				continue
			}
			log.Infof("MonitorChain - fabric height is %d", height)
			for e.currentHeight < height - e.config.FabricConfig.BlockConfig {
				blockHandleResult := e.HandleNewBlock(e.currentHeight + 1)
				if blockHandleResult == false {
					break
				}
				e.currentHeight++
			}
		}
	}
}

func (e *FabricManager) HandleNewBlock(height uint64) bool {
	events, err := e.client.GetCrossChainEvent(height)
	if err != nil {
		log.Errorf("get cross chain event err: %v", err)
		return false
	}
	for _, event := range events {
		e.commitCrossChainEvent(uint32(height), event.Data, event.TxHash)
	}
	return true
}

func (e *FabricManager) commitCrossChainEvent(height uint32, value []byte, txhash []byte) (string, error) {
	log.Debugf("commit proof, height: %d, value: %s, txhash: %s", height, hex.EncodeToString(value), hex.EncodeToString(txhash))

	hash := crypto.SHA256.New()
	hash.Write(value)
	digest := hash.Sum(nil)

	sigs := make([][]byte, len(e.fabPrivks))
	for i, k := range e.fabPrivks {
		sig, err := k.Sign(rand.Reader, digest, nil)
		if err != nil {
			log.Errorf("No.%d failed to sign: %v", i, err)
			return "", err
		}
		sigs[i] = sig
	}

	sink := common.NewZeroCopySink(nil)
	e.multiTrustChain.Serialization(sink)

	tx, err := e.polySdk.Native.Ccm.ImportOuterTransfer(
		e.config.FabricConfig.SideChainId,
		value,
		height,
		fabric.SigArrSerialize(sigs),
		e.polySigner.Address[:],
		sink.Bytes(),
		e.polySigner)
	if err != nil {
		log.Errorf("commitProof err: %v", err)
		return "", err
	} else {
		log.Infof("commitProof - send transaction to poly chain: ( poly_txhash: %s, fabric_txhash: %s, height: %d )",
			tx.ToHexString(), common.ToHexString(txhash), height)
		return tx.ToHexString(), nil
	}
}

func (e *FabricManager) Test() {
	for true {
		time.Sleep(time.Second * 30)
		e.client.Lock()
	}
}
