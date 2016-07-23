package uspv

import (
	"bytes"
	"fmt"

	"github.com/lightningnetwork/lnd/elkrem"

	"github.com/btcsuite/fastsha256"
	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcd/txscript"
	"github.com/roasbeef/btcd/wire"
	"github.com/roasbeef/btcutil"
	"github.com/roasbeef/btcutil/txsort"
)

const (
	// high 3 bytes are in sequence, low 3 bytes are in time
	seqMask  = 0xff000000 // assert high byte
	timeMask = 0x21000000 // 1987 to 1988

	MSGID_POINTREQ  = 0x30
	MSGID_POINTRESP = 0x31
	MSGID_CHANDESC  = 0x32
	MSGID_CHANACK   = 0x33
	MSGID_SIGPROOF  = 0x34

	MSGID_CLOSEREQ  = 0x40
	MSGID_CLOSERESP = 0x41

	MSGID_TEXTCHAT = 0x70

	MSGID_RTS    = 0x80 // pushing funds in channel; request to send
	MSGID_ACKSIG = 0x81 // pulling funds in channel; acknowledge update and sign
	MSGID_SIGREV = 0x82 // pushing funds; signing new state and revoking old
	MSGID_REVOKE = 0x83 // pulling funds; revoking previous channel state

	MSGID_FWDMSG     = 0x20
	MSGID_FWDAUTHREQ = 0x21
)

// Uhh, quick channel.  For now.  Once you get greater spire it upgrades to
// a full channel that can do everything.
type Qchan struct {
	// S for stored (on disk), D for derived

	Utxo                 // S underlying utxo data
	CloseData QCloseData // S closing outpoint

	MyPub    [33]byte // D my channel specific pubkey
	TheirPub [33]byte // S their channel specific pubkey

	PeerId [33]byte // D useful for quick traverse of db

	//TODO can make refund a 20 byte pubkeyhash
	MyRefundPub    [33]byte // D my refund pubkey for channel break
	TheirRefundPub [33]byte // S their pubkey for channel break

	MyHAKDBase    [33]byte // D my base point for HAKD and timeout keys
	TheirHAKDBase [33]byte // S their base point for HAKD and timeout keys

	// Elkrem is used for revoking state commitments
	ElkSnd *elkrem.ElkremSender   // D derived from channel specific key
	ElkRcv *elkrem.ElkremReceiver // S stored in db

	TimeOut uint16 // blocks for timeout (default 5 for testing)

	State *StatCom // S state of channel
}

// StatComs are State Commitments.
// all elements are saved to the db.
type StatCom struct {
	StateIdx uint64 // this is the n'th state commitment

	MyAmt int64 // my channel allocation
	// their Amt is the utxo.Value minus this
	Delta int32 // fun amount in-transit; is negative for the pusher

	// Homomorphic Adversarial Key Derivation public keys (HAKD)
	MyHAKDPub     [33]byte // saved to disk
	MyPrevHAKDPub [33]byte // When you haven't gotten their revocation elkrem yet.

	sig []byte // Counterparty's signature (for StatCom tx)
	// don't write to sig directly; only overwrite via

	// note sig can be nil during channel creation. if stateIdx isn't 0,
	// sig should have a sig.
	// only one sig is ever stored, to prevent broadcasting the wrong tx.
	// could add a mutex here... maybe will later.
}

// QCloseData is the output resulting from an un-cooperative close
// of the channel.  This happens when either party breaks non-cooperatively.
// It describes "your" output, either pkh or time-delay script.
// If you have pkh but can grab the other output, "grabbable" is set to true.
// This can be serialized in a separate bucket

type QCloseData struct {
	// 3 txid / height pairs are stored.  All 3 only are used in the
	// case where you grab their invalid close.
	CloseTxid   wire.ShaHash
	CloseHeight int32
	Closed      bool // if channel is closed; if CloseTxid != -1
}

// GetStateIdxFromTx returns the state index from a commitment transaction.
// No errors; returns 0 if there is no retrievable index.
// Takes the xor input X which is derived from the 0th elkrems.
func GetStateIdxFromTx(tx *wire.MsgTx, x uint64) uint64 {
	// no tx, so no index
	if tx == nil {
		return 0
	}
	// more than 1 input, so not a close tx
	if len(tx.TxIn) != 1 {
		return 0
	}
	if x >= 1<<48 {
		return 0
	}
	// check that indicating high bytes are correct
	if tx.TxIn[0].Sequence>>24 != 0xff || tx.LockTime>>24 != 0x21 {
		//		fmt.Printf("sequence byte %x, locktime byte %x\n",
		//			tx.TxIn[0].Sequence>>24, tx.LockTime>>24 != 0x21)
		return 0
	}
	// high 24 bits sequence, low 24 bits locktime
	seqBits := uint64(tx.TxIn[0].Sequence & 0x00ffffff)
	timeBits := uint64(tx.LockTime & 0x00ffffff)

	return (seqBits<<24 | timeBits) ^ x
}

// SetStateIdxBits modifies the tx in place, setting the sequence and locktime
// fields to indicate the given state index.
func SetStateIdxBits(tx *wire.MsgTx, idx, x uint64) error {
	if tx == nil {
		return fmt.Errorf("SetStateIdxBits: nil tx")
	}
	if len(tx.TxIn) != 1 {
		return fmt.Errorf("SetStateIdxBits: tx has %d inputs", len(tx.TxIn))
	}
	if idx >= 1<<48 {
		return fmt.Errorf(
			"SetStateIdxBits: index %d greater than max %d", idx, uint64(1<<48)-1)
	}

	idx = idx ^ x
	// high 24 bits sequence, low 24 bits locktime
	seqBits := uint32(idx >> 24)
	timeBits := uint32(idx & 0x00ffffff)

	tx.TxIn[0].Sequence = seqBits | seqMask
	tx.LockTime = timeBits | timeMask

	return nil
}

// GetCloseTxos takes in a tx and sets the QcloseTXO feilds based on the tx.
// It also returns the spendable (u)txos generated by the close.
func (q *Qchan) GetCloseTxos(tx *wire.MsgTx) ([]Utxo, error) {
	if tx == nil {
		return nil, fmt.Errorf("IngesGetCloseTxostCloseTx: nil tx")
	}
	txid := tx.TxSha()
	// double check -- does this tx actually close the channel?
	if !(len(tx.TxIn) == 1 && OutPointsEqual(tx.TxIn[0].PreviousOutPoint, q.Op)) {
		return nil, fmt.Errorf("tx %s doesn't spend channel outpoint %s",
			txid.String(), q.Op.String())
	}
	// hardcode here now... need to save to qchan struct I guess
	q.TimeOut = 5
	x := q.GetElkZeroOffset()
	if x >= 1<<48 {
		return nil, fmt.Errorf("GetCloseTxos elkrem error, x= %x", x)
	}
	// first, check if cooperative
	txIdx := GetStateIdxFromTx(tx, x)
	if txIdx == 0 || len(tx.TxOut) != 2 {
		// must have been cooperative, or something else we don't recognize
		// if simple close, still have a PKH output, find it.
		// so far, assume 1 txo
		var pkhTxo Utxo
		for i, out := range tx.TxOut {
			if len(out.PkScript) < 22 {
				continue // skip to prevent crash
			}
			if bytes.Equal(
				out.PkScript[2:22], btcutil.Hash160(q.MyRefundPub[:])) {
				pkhTxo.Op.Hash = txid
				pkhTxo.Op.Index = uint32(i)
				pkhTxo.AtHeight = q.CloseData.CloseHeight
				pkhTxo.KeyIdx = q.KeyIdx
				pkhTxo.PeerIdx = q.PeerIdx
				pkhTxo.Value = tx.TxOut[i].Value
				pkhTxo.SpendLag = 1 // 1 for witness, non time locked
				return []Utxo{pkhTxo}, nil
			}
		}
		// couldn't find anything... shouldn't happen
		return nil, nil
	}
	var shIdx, pkhIdx uint32
	cTxos := make([]Utxo, 1)
	// still here, so not cooperative. sort outputs into PKH and SH
	if len(tx.TxOut[0].PkScript) == 34 {
		shIdx = 0
		pkhIdx = 1
	} else {
		pkhIdx = 0
		shIdx = 1
	}
	// make sure PKH output is actually PKH
	if len(tx.TxOut[pkhIdx].PkScript) != 22 {
		return nil, fmt.Errorf("non-p2wsh output is length %d, expect 22",
			len(tx.TxOut[pkhIdx].PkScript))
	}

	// next, check if SH is mine (implied by PKH is not mine)
	if !bytes.Equal(
		tx.TxOut[pkhIdx].PkScript[2:22], btcutil.Hash160(q.MyRefundPub[:])) {
		// ------------pkh not mine; sh is mine
		// note that this doesn't actually check that the SH script is correct.
		// could add that in to double check.

		var shTxo Utxo // create new utxo and copy into it
		shTxo.Op.Hash = txid
		shTxo.Op.Index = shIdx
		shTxo.AtHeight = q.CloseData.CloseHeight
		shTxo.KeyIdx = q.KeyIdx
		shTxo.PeerIdx = q.PeerIdx
		shTxo.Value = tx.TxOut[shIdx].Value
		shTxo.SpendLag = int32(q.TimeOut)
		cTxos[0] = shTxo
		// if SH is mine we're done
		return cTxos, nil
	}
	// ----------pkh is mine
	var pkhTxo Utxo // create new utxo and copy into it
	pkhTxo.Op.Hash = txid
	pkhTxo.Op.Index = pkhIdx
	pkhTxo.AtHeight = q.CloseData.CloseHeight
	pkhTxo.KeyIdx = q.KeyIdx
	pkhTxo.PeerIdx = q.PeerIdx
	pkhTxo.Value = tx.TxOut[pkhIdx].Value
	pkhTxo.SpendLag = 1 // 1 for witness, non time locked
	cTxos[0] = pkhTxo

	// OK, it's my PKH, but can I grab the SH???
	if txIdx < q.State.StateIdx {
		// invalid previous state, can be grabbed!
		var shTxo Utxo // create new utxo and copy into it
		shTxo.Op.Hash = txid
		shTxo.Op.Index = shIdx
		shTxo.AtHeight = q.CloseData.CloseHeight
		// note that these key indexes are not sufficient to grab;
		// the grabbable utxo is more of an indicator; the HAKD will need
		// to be loaded from the DB to grab.
		shTxo.KeyIdx = q.KeyIdx
		shTxo.PeerIdx = q.PeerIdx
		shTxo.Value = tx.TxOut[shIdx].Value
		shTxo.SpendLag = -1
		cTxos = append(cTxos, shTxo)
	}

	//	if txIdx > q.State.StateIdx {
	// invalid FUTURE state.  Is this even an error..?
	// don't error for now.  can't do anything anyway.
	//	}

	return cTxos, nil
}

// ChannelInfo prints info about a channel.
func (t *TxStore) QchanInfo(q *Qchan) error {
	// display txid instead of outpoint because easier to copy/paste
	fmt.Printf("CHANNEL %s h:%d (%d,%d) cap: %d\n",
		q.Op.Hash.String(), q.AtHeight, q.PeerIdx, q.KeyIdx, q.Value)
	fmt.Printf("\tPUB mine:%x them:%x REFUND mine:%x them:%x\n",
		q.MyPub[:4], q.TheirPub[:4], q.MyRefundPub[:4], q.TheirRefundPub[:4])
	if q.State == nil || q.ElkRcv == nil {
		fmt.Printf("\t no valid state or elkrem\n")
	} else {

		fmt.Printf("\ta %d (them %d) state index %d\n",
			q.State.MyAmt, q.Value-q.State.MyAmt, q.State.StateIdx)
		fmt.Printf("\tdelta:%d HAKD:%x prevHAKD:%x elk@ %d\n",
			q.State.Delta, q.State.MyHAKDPub[:4], q.State.MyPrevHAKDPub[:4],
			q.ElkRcv.UpTo())
	}

	if !q.CloseData.Closed { // still open, finish here
		return nil
	}

	fmt.Printf("\tCLOSED at height %d by tx: %s\n",
		q.CloseData.CloseHeight, q.CloseData.CloseTxid.String())
	clTx, err := t.GetTx(&q.CloseData.CloseTxid)
	if err != nil {
		return err
	}
	ctxos, err := q.GetCloseTxos(clTx)
	if err != nil {
		return err
	}

	if len(ctxos) == 0 {
		fmt.Printf("\tcooperative close.\n")
		return nil
	}

	fmt.Printf("\tClose resulted in %d spendable txos\n", len(ctxos))
	if len(ctxos) == 2 {
		fmt.Printf("\t\tINVALID CLOSE!!!11\n")
	}
	for i, u := range ctxos {
		fmt.Printf("\t\t%d) amt: %d spendable: %d\n", i, u.Value, u.SpendLag)
	}
	return nil
}

// GrabTx produces the "remedy" transaction to get all the money if they
// broadcast an old state which they invalidated.
// This function assumes a recovery is possible; if it can't construct the right
// keys and scripts it will return an error.
func (t *TxStore) GrabUtxo(u *Utxo) (*wire.MsgTx, error) {
	if u == nil {
		return nil, fmt.Errorf("Grab error: nil utxo")
	}
	// this utxo is returned by PickUtxos() so should be ready to spend
	// first get the channel data
	qc, err := t.GetQchanByIdx(u.PeerIdx, u.KeyIdx)
	if err != nil {
		return nil, err
	}

	// load closing tx
	closeTx, err := t.GetTx(&qc.CloseData.CloseTxid)
	if err != nil {
		return nil, err
	}
	if len(closeTx.TxOut) != 2 { // (could be more later; onehop is 2)
		return nil, fmt.Errorf("close tx has %d outputs, can't grab",
			len(closeTx.TxOut))
	}
	if len(closeTx.TxOut[u.Op.Index].PkScript) != 34 {
		return nil, fmt.Errorf("grab txout pkscript length %d, expect 34",
			len(closeTx.TxOut[u.Op.Index].PkScript))
	}

	x := qc.GetElkZeroOffset()
	if x >= 1<<48 {
		return nil, fmt.Errorf("GrabUtxo elkrem error, x= %x", x)
	}
	// find state index based on tx hints (locktime / sequence)
	txIdx := GetStateIdxFromTx(closeTx, x)
	if txIdx == 0 {
		return nil, fmt.Errorf("no hint, can't recover")
	}

	//	t.GrabTx(qc, txIdx)
	shOut := closeTx.TxOut[u.Op.Index]
	// if hinted state is greater than elkrem state we can't recover
	if txIdx > qc.ElkRcv.UpTo() {
		return nil, fmt.Errorf("tx at state %d but elkrem only goes to %d",
			txIdx, qc.ElkRcv.UpTo())
	}

	elk, err := qc.ElkRcv.AtIndex(txIdx)
	if err != nil {
		return nil, err
	}
	fmt.Printf("made elk %s at index %d\n", elk.String(), txIdx)

	// get private signing key
	priv := t.GetRefundPrivkey(qc.PeerIdx, qc.KeyIdx)
	fmt.Printf("made chan pub %x\n", priv.PubKey().SerializeCompressed())
	// modify private key
	PrivKeyAddBytes(priv, elk.Bytes())

	// serialize pubkey part for script generation
	var HAKDpubArr [33]byte
	copy(HAKDpubArr[:], priv.PubKey().SerializeCompressed())
	fmt.Printf("made HAKD to recover from %x\n", HAKDpubArr)

	// now that everything is chosen, build fancy script and pkh script
	preScript, _ := CommitScript2(HAKDpubArr, qc.TheirRefundPub, qc.TimeOut)
	fancyScript := P2WSHify(preScript) // p2wsh-ify
	fmt.Printf("prescript: %x\np2wshd: %x\n", preScript, fancyScript)
	if !bytes.Equal(fancyScript, shOut.PkScript) {
		return nil, fmt.Errorf("script hash mismatch, generated %x expect %x",
			fancyScript, shOut.PkScript)
	}

	// build tx and sign.
	sweepTx := wire.NewMsgTx()
	destTxOut, err := t.NewChangeOut(shOut.Value - 5000) // fixed fee for now
	if err != nil {
		return nil, err
	}
	sweepTx.AddTxOut(destTxOut)

	// add unsigned input
	sweepIn := wire.NewTxIn(&u.Op, nil, nil)
	sweepTx.AddTxIn(sweepIn)

	// make hash cache for this tx
	hCache := txscript.NewTxSigHashes(sweepTx)

	// sign
	sig, err := txscript.RawTxInWitnessSignature(
		sweepTx, hCache, 0, shOut.Value, preScript, txscript.SigHashAll, priv)

	sweepTx.TxIn[0].Witness = make([][]byte, 2)
	sweepTx.TxIn[0].Witness[0] = sig
	sweepTx.TxIn[0].Witness[1] = preScript
	// that's it...?

	return sweepTx, nil
}

// GetElkZeroOffset returns a 48-bit uint (cast up to 8 bytes) based on the sender
// and receiver elkrem at index 0.  If there's an error, it returns ff...
func (q *Qchan) GetElkZeroOffset() uint64 {
	theirZero, err := q.ElkRcv.AtIndex(0)
	if err != nil {
		fmt.Printf(err.Error())
		return 0xffffffffffffffff
	}
	myZero, err := q.ElkSnd.AtIndex(0)
	if err != nil {
		fmt.Printf(err.Error())
		return 0xffffffffffffffff
	}
	theirBytes := theirZero.Bytes()
	myBytes := myZero.Bytes()
	x := make([]byte, 8)
	for i := 2; i < 8; i++ {
		x[i] = myBytes[i] ^ theirBytes[i]
	}

	// only 48 bits so will be OK when cast to signed 64 bit
	return uint64(BtI64(x[:]))
}

// MakeTheirHAKDMyTimeout makes their HAKD pubey, and my timeout pubkey
// for the current state.  Their HAKD pub is their base point times my
// current state elk; my timeout key is my base point times the hash
// of the HAKD pub I just made for them.
// idx the index at which to generate pubkeys (usually the current state index)
func (q *Qchan) MakeTheirHAKDMyTimeout(
	idx uint64) (hakd, timeout [33]byte, err error) {
	// sanity check
	if q == nil || q.ElkSnd == nil { // can't do anything
		err = fmt.Errorf("can't access elkrem")
		return
	}
	var elk *wire.ShaHash
	elk, err = q.ElkSnd.AtIndex(idx)
	if err != nil {
		return
	}

	// start with the base points
	hakd = q.TheirHAKDBase
	timeout = q.MyHAKDBase

	// add my still secret elkrem (point) to their base point
	err = PubKeyArrAddBytes(&hakd, elk.Bytes())
	if err != nil {
		return
	}
	// my timeout key is my base point plus the hash of their hakd
	err = PubKeyArrAddBytes(&timeout, wire.DoubleSha256(hakd[:]))

	return
}

// MakeMyHAKDTheirTimeout makes my HAKD pubkey and their timeout key
// for the current state.
// idx the index at which to generate pubkeys (usually the current state index)
func (q *Qchan) MakeMyHAKDTheirTimeout(
	idx uint64) (hakd, timeout [33]byte, err error) {
	// sanity check
	if q == nil || q.ElkRcv == nil { // can't do anything
		err = fmt.Errorf("can't access elkrem")
		return
	}
	var elk *wire.ShaHash
	elk, err = q.ElkRcv.AtIndex(idx)
	if err != nil {
		return
	}

	// start with the base points
	hakd = q.MyHAKDBase
	timeout = q.TheirHAKDBase

	// add my their revealed elkrem (point) to my base point
	err = PubKeyArrAddBytes(&hakd, elk.Bytes())
	if err != nil {
		return
	}
	// their timeout key is their base point plus the hash of my hakd
	err = PubKeyArrAddBytes(&timeout, wire.DoubleSha256(hakd[:]))

	return
}

// MakeHAKDPubkey generates the HAKD pubkey to send out or everify sigs.
// leaves channel struct the same; returns HAKD pubkey.
func (q *Qchan) MakeTheirHAKDPubkey() ([33]byte, error) {
	var HAKDPubArr [33]byte

	// sanity check
	if q == nil || q.ElkSnd == nil { // can't do anything
		var empty [33]byte
		return empty, fmt.Errorf("can't access elkrem")
	}

	// use the elkrem sender at state's index.  not index + 1
	// (you revoke index - 1)
	elk, err := q.ElkSnd.AtIndex(q.State.StateIdx)
	if err != nil {
		return HAKDPubArr, err
	}

	// copy their pubkey for modification
	HAKDPubArr = q.TheirHAKDBase

	// add our elkrem to their channel pubkey
	err = PubKeyArrAddBytes(&HAKDPubArr, elk.Bytes())

	return HAKDPubArr, err
}

// MakeMyTimeoutKey genreates my time-lock pubkey based on the last
// sent elkrem hash and my HAKD base point.
func (q *Qchan) MakeMyTimeoutKey() ([33]byte, error) {
	var empty [33]byte

	// sanity check
	if q == nil || q.ElkSnd == nil { // can't do anything
		return empty, fmt.Errorf("can't access elkrem")
	}

	// use the elkrem sender at state's index-1.
	fmt.Printf("MakeMyTimeoutKey make %d\n", q.State.StateIdx-1)
	elk, err := q.ElkSnd.AtIndex(q.State.StateIdx - 1)
	if err != nil {
		return empty, err
	}

	// copy my HAKD base for modification
	timePubArr := q.MyHAKDBase

	// add our elkrem to my HAKD base point
	err = PubKeyArrAddBytes(&timePubArr, elk.Bytes())

	return timePubArr, err
}

// MakeTheirTimeoutKey genreates their time-lock pubkey based on the last
// received elkrem hash and my HAKD base point.
func (q *Qchan) MakeTheirTimeoutKey() ([33]byte, error) {
	var empty [33]byte

	// sanity check
	if q == nil || q.ElkRcv == nil { // can't do anything
		return empty, fmt.Errorf("can't access elkrem")
	}
	fmt.Printf("MakeTheirTimeoutKey make %d\n", q.State.StateIdx-1)
	// use the elkrem sender at state's index-2.
	elk, err := q.ElkRcv.AtIndex(q.State.StateIdx - 1)
	if err != nil {
		return empty, err
	}

	// copy my HAKD base for modification
	timePubArr := q.TheirHAKDBase

	// add our elkrem to my HAKD base point
	err = PubKeyArrAddBytes(&timePubArr, elk.Bytes())

	return timePubArr, err
}

// IngestElkrem takes in an elkrem hash, performing 2 checks:
// that it produces the proper HAKD key, and that it fits into the elkrem tree.
// if both of these are the case it updates the channel state, removing the
// revoked HAKD. If either of these checks fail, and definitely the second one
// fails, I'm pretty sure the channel is not recoverable and needs to be closed.
func (q *Qchan) IngestElkrem(elk *wire.ShaHash) error {
	if elk == nil {
		return fmt.Errorf("IngestElkrem: nil hash")
	}

	// first verify if the elkrem produces the previous HAKD's PUBLIC key.
	// We don't actually use the private key operation here, because we can
	// do the same operation on our pubkey that they did, and we have faith
	// in the mysterious power of abelian group homomorphisms that the private
	// key modification will also work.

	// first verify the elkrem insertion (this only performs checks 1/2 the time, so
	// 1/2 the time it'll work even if the elkrem is invalid, oh well)
	err := q.ElkRcv.AddNext(elk)
	if err != nil {
		return err
	}
	fmt.Printf("ingested hash, receiver now has up to %d\n", q.ElkRcv.UpTo())

	// if this is state 1, this is elkrem 0 and we can stop here.
	// there's nothing to revoke. (state 0, also? but that would imply
	// elkrem -1 which isn't a thing... so fail in that case.)
	if q.State.StateIdx == 1 {
		return nil
	}

	var PubArr, empty [33]byte
	PubArr = q.MyHAKDBase
	err = PubKeyArrAddBytes(&PubArr, elk.Bytes())
	if err != nil {
		return err
	}
	// see if it matches my previous HAKD pubkey
	if PubArr != q.State.MyPrevHAKDPub {
		// didn't match, the whole channel is borked.
		return fmt.Errorf("Provided elk doesn't create HAKD pub %x! Need to close",
			q.State.MyPrevHAKDPub)
	}

	// it did match, so we can clear the previous HAKD pub
	q.State.MyPrevHAKDPub = empty

	return nil
}

// SignBreak signs YOUR tx, which you already have a sig for
func (t TxStore) SignBreakTx(q *Qchan) (*wire.MsgTx, error) {
	// generate their HAKDpub.  Be sure you haven't revoked it!
	theirHAKDpub, _, err := q.MakeTheirHAKDMyTimeout(q.State.StateIdx)
	if err != nil {
		return nil, err
	}

	//	theirHAKDpub, err := q.MakeTheirHAKDPubkey()

	tx, err := q.BuildStateTx(theirHAKDpub)
	if err != nil {
		return nil, err
	}

	// make hash cache for this tx
	hCache := txscript.NewTxSigHashes(tx)

	// generate script preimage (keep track of key order)
	pre, swap, err := FundTxScript(q.MyPub, q.TheirPub)
	if err != nil {
		return nil, err
	}

	// get private signing key
	priv := t.GetChanPrivkey(q.PeerIdx, q.KeyIdx)
	// generate sig.
	mySig, err := txscript.RawTxInWitnessSignature(
		tx, hCache, 0, q.Value, pre, txscript.SigHashAll, priv)

	// put the sighash all byte on the end of their signature
	// copy here because... otherwise I get unexpected fault address 0x...
	theirSig := make([]byte, len(q.State.sig)+1)
	copy(theirSig, q.State.sig)
	theirSig[len(theirSig)-1] = byte(txscript.SigHashAll)

	fmt.Printf("made mysig: %x theirsig: %x\n", mySig, theirSig)
	// add sigs to the witness stack
	if swap {
		tx.TxIn[0].Witness = SpendMultiSigWitStack(pre, theirSig, mySig)
	} else {
		tx.TxIn[0].Witness = SpendMultiSigWitStack(pre, mySig, theirSig)
	}
	return tx, nil
}

// SimpleCloseTx produces a close tx based on the current state.
// When
func (q *Qchan) SimpleCloseTx() *wire.MsgTx {
	// sanity checks
	if q == nil || q.State == nil {
		fmt.Printf("SimpleCloseTx: nil chan / state")
		return nil
	}
	fee := int64(5000) // fixed fee for now (on both sides)

	// make my output
	myScript := DirectWPKHScript(q.MyRefundPub)
	myOutput := wire.NewTxOut(q.State.MyAmt-fee, myScript)
	// make their output
	theirScript := DirectWPKHScript(q.TheirRefundPub)
	theirOutput := wire.NewTxOut((q.Value-q.State.MyAmt)-fee, theirScript)

	// make tx with these outputs
	tx := wire.NewMsgTx()
	tx.AddTxOut(myOutput)
	tx.AddTxOut(theirOutput)
	// add channel outpoint as txin
	tx.AddTxIn(wire.NewTxIn(&q.Op, nil, nil))
	// sort and return
	txsort.InPlaceSort(tx)
	return tx
}

// SignSimpleClose creates a close tx based on the current state and signs it,
// returning that sig.  Also returns a bool; true means this sig goes second.
func (t TxStore) SignSimpleClose(q *Qchan) ([]byte, error) {
	tx := q.SimpleCloseTx()
	if tx == nil {
		return nil, fmt.Errorf("SignSimpleClose: no tx")
	}
	// make hash cache
	hCache := txscript.NewTxSigHashes(tx)

	// generate script preimage for signing (ignore key order)
	pre, _, err := FundTxScript(q.MyPub, q.TheirPub)
	if err != nil {
		return nil, err
	}
	// get private signing key
	priv := t.GetChanPrivkey(q.PeerIdx, q.KeyIdx)
	// generate sig
	sig, err := txscript.RawTxInWitnessSignature(
		tx, hCache, 0, q.Value, pre, txscript.SigHashAll, priv)
	if err != nil {
		return nil, err
	}

	return sig, nil
}

// SignNextState generates your signature for their state. (usually)
func (t TxStore) SignState(q *Qchan) ([]byte, error) {
	var empty [33]byte
	// build transaction for next state
	tx, err := q.BuildStateTx(empty) // generally their tx, as I'm signing
	if err != nil {
		return nil, err
	}

	// make hash cache for this tx
	hCache := txscript.NewTxSigHashes(tx)

	// generate script preimage (ignore key order)
	pre, _, err := FundTxScript(q.MyPub, q.TheirPub)
	if err != nil {
		return nil, err
	}

	// get private signing key
	priv := t.GetChanPrivkey(q.PeerIdx, q.KeyIdx)

	// generate sig.
	sig, err := txscript.RawTxInWitnessSignature(
		tx, hCache, 0, q.Value, pre, txscript.SigHashAll, priv)
	// truncate sig (last byte is sighash type, always sighashAll)
	sig = sig[:len(sig)-1]

	fmt.Printf("____ sig creation for channel (%d,%d):\n", q.PeerIdx, q.KeyIdx)
	fmt.Printf("\tinput %s\n", tx.TxIn[0].PreviousOutPoint.String())
	fmt.Printf("\toutput 0: %x %d\n", tx.TxOut[0].PkScript, tx.TxOut[0].Value)
	fmt.Printf("\toutput 1: %x %d\n", tx.TxOut[1].PkScript, tx.TxOut[1].Value)
	fmt.Printf("\tstate %d myamt: %d theiramt: %d\n", q.State.StateIdx, q.State.MyAmt, q.Value-q.State.MyAmt)
	fmt.Printf("\tmy HAKD pub: %x their HAKD pub: %x sig: %x\n", q.State.MyHAKDPub[:4], empty[:4], sig)

	return sig, nil
}

// VerifySig verifies their signature for your next state.
// it also saves the sig if it's good.
// do bool, error or just error?  Bad sig is an error I guess.
// for verifying signature, always use theirHAKDpub, so generate & populate within
// this function.
func (q *Qchan) VerifySig(sig []byte) error {
	theirHAKDpub, err := q.MakeTheirHAKDPubkey()
	if err != nil {
		fmt.Printf("ACKSIGHandler err %s", err.Error())
		return err
	}

	// ALWAYS my tx, ALWAYS their HAKD when I'm verifying.
	tx, err := q.BuildStateTx(theirHAKDpub)
	if err != nil {
		return err
	}

	// generate fund output script preimage (ignore key order)
	pre, _, err := FundTxScript(q.MyPub, q.TheirPub)
	if err != nil {
		return err
	}

	hCache := txscript.NewTxSigHashes(tx)
	// always sighash all
	hash, err := txscript.CalcWitnessSigHash(
		pre, hCache, txscript.SigHashAll, tx, 0, q.Value)
	if err != nil {
		return err
	}

	// sig is pre-truncated; last byte for sighashtype is always sighashAll
	pSig, err := btcec.ParseDERSignature(sig, btcec.S256())
	if err != nil {
		return err
	}
	theirPubKey, err := btcec.ParsePubKey(q.TheirPub[:], btcec.S256())
	if err != nil {
		return err
	}
	fmt.Printf("____ sig verification for channel (%d,%d):\n", q.PeerIdx, q.KeyIdx)
	fmt.Printf("\tinput %s\n", tx.TxIn[0].PreviousOutPoint.String())
	fmt.Printf("\toutput 0: %x %d\n", tx.TxOut[0].PkScript, tx.TxOut[0].Value)
	fmt.Printf("\toutput 1: %x %d\n", tx.TxOut[1].PkScript, tx.TxOut[1].Value)
	fmt.Printf("\tstate %d myamt: %d theiramt: %d\n", q.State.StateIdx, q.State.MyAmt, q.Value-q.State.MyAmt)
	fmt.Printf("\tmy HAKD pub: %x their HAKD pub: %x sig: %x\n", q.State.MyHAKDPub[:4], theirHAKDpub[:4], sig)

	worked := pSig.Verify(hash, theirPubKey)
	if !worked {
		return fmt.Errorf("Their sig was no good!!!!!111")
	}

	// copy signature, overwriting old signature.
	q.State.sig = sig

	return nil
}

// BuildStateTx constructs and returns a state tx.  As simple as I can make it.
// This func just makes the tx with data from State in ram, and HAKD key arg
// Delta should always be 0 when making this tx.
// It decides whether to make THEIR tx or YOUR tx based on the HAKD pubkey given --
// if it's zero, then it makes their transaction (for signing onlu)
// If it's full, it makes your transaction (for verification in most cases,
// but also for signing when breaking the channel)
// Index is used to set nlocktime for state hints.
// fee and op_csv timeout are currently hardcoded, make those parameters later.
// also returns the script preimage for later spending.
func (q *Qchan) BuildStateTx(theirHAKDpub [33]byte) (*wire.MsgTx, error) {
	if q == nil {
		return nil, fmt.Errorf("BuildStateTx: nil chan")
	}
	// sanity checks
	s := q.State // use it a lot, make shorthand variable
	if s == nil {
		return nil, fmt.Errorf("channel (%d,%d) has no state", q.PeerIdx, q.KeyIdx)
	}
	// if delta is non-zero, something is wrong.
	if s.Delta != 0 {
		return nil, fmt.Errorf(
			"BuildStateTx: delta is %d (expect 0)", s.Delta)
	}

	var empty [33]byte
	var err error
	var fancyAmt, pkhAmt int64   // output amounts
	var revPub, timePub [33]byte // pubkeys
	var pkhPub [33]byte          // the simple output's pub key hash
	fee := int64(5000)           // fixed fee for now
	delay := uint16(5)           // fixed CSV delay for now
	// delay is super short for testing.

	if theirHAKDpub == empty { // TheirHAKDPub is empty; build THEIR tx (to sign)
		// Their tx that they store.  I get funds unencumbered.
		pkhPub = q.MyRefundPub
		pkhAmt = s.MyAmt - fee

		// make timepub: their refund pub with their elk of state-1
		timePub, err = q.MakeTheirTimeoutKey()
		if err != nil {
			return nil, err
		}
		revPub = s.MyHAKDPub // if they're given me the elkrem, it's mine
		fancyAmt = (q.Value - s.MyAmt) - fee
	} else { // theirHAKDPub is full; build MY tx (to verify) (unless breaking)
		// My tx that I store.  They get funds unencumbered.
		pkhPub = q.TheirRefundPub
		pkhAmt = (q.Value - s.MyAmt) - fee

		// make timepub: my refund pub with my elk of state-1
		timePub, err = q.MakeMyTimeoutKey()
		if err != nil {
			return nil, err
		}
		revPub = theirHAKDpub // I can revoke by giving them the elkrem
		fancyAmt = s.MyAmt - fee
	}

	// now that everything is chosen, build fancy script and pkh script
	fancyScript, _ := CommitScript2(revPub, timePub, delay)
	pkhScript := DirectWPKHScript(pkhPub) // p2wpkh-ify
	fancyScript = P2WSHify(fancyScript)   // p2wsh-ify

	// create txouts by assigning amounts
	outFancy := wire.NewTxOut(fancyAmt, fancyScript)
	outPKH := wire.NewTxOut(pkhAmt, pkhScript)

	// make a new tx
	tx := wire.NewMsgTx()
	// add txouts
	tx.AddTxOut(outFancy)
	tx.AddTxOut(outPKH)
	// add unsigned txin
	tx.AddTxIn(wire.NewTxIn(&q.Op, nil, nil))
	// set index hints
	var x uint64
	if s.StateIdx > 0 { // state 0 and 1 can't use xor'd elkrem... fix this?
		x = q.GetElkZeroOffset()
		if x >= 1<<48 {
			return nil, fmt.Errorf("BuildStateTx elkrem error, x= %x", x)
		}
	}
	SetStateIdxBits(tx, s.StateIdx, x)

	// sort outputs
	txsort.InPlaceSort(tx)
	return tx, nil
}

func DirectWPKHScript(pub [33]byte) []byte {
	builder := txscript.NewScriptBuilder()
	builder.AddOp(txscript.OP_0).AddData(btcutil.Hash160(pub[:]))
	b, _ := builder.Script()
	return b
}

// CommitScript2 doesn't use hashes, but a modified pubkey.
// To spend from it, push your sig.  If it's time-based,
// you have to set the txin's sequence.
func CommitScript2(RKey, TKey [33]byte, delay uint16) ([]byte, error) {
	builder := txscript.NewScriptBuilder()

	builder.AddOp(txscript.OP_DUP)
	builder.AddData(RKey[:])
	builder.AddOp(txscript.OP_CHECKSIG)

	builder.AddOp(txscript.OP_NOTIF)

	builder.AddData(TKey[:])
	builder.AddOp(txscript.OP_CHECKSIGVERIFY)
	builder.AddInt64(int64(delay))
	builder.AddOp(txscript.OP_CHECKSEQUENCEVERIFY)

	builder.AddOp(txscript.OP_ENDIF)

	return builder.Script()
}

// FundMultiOut creates a TxOut for the funding transaction.
// Give it the two pubkeys and it'll give you the p2sh'd txout.
// You don't have to remember the p2sh preimage, as long as you remember the
// pubkeys involved.
func FundTxOut(pubA, puB [33]byte, amt int64) (*wire.TxOut, error) {
	if amt < 0 {
		return nil, fmt.Errorf("Can't create FundTx script with negative coins")
	}
	scriptBytes, _, err := FundTxScript(pubA, puB)
	if err != nil {
		return nil, err
	}
	scriptBytes = P2WSHify(scriptBytes)

	return wire.NewTxOut(amt, scriptBytes), nil
}

// FundMultiPre generates the non-p2sh'd multisig script for 2 of 2 pubkeys.
// useful for making transactions spending the fundtx.
// returns a bool which is true if swapping occurs.
func FundTxScript(aPub, bPub [33]byte) ([]byte, bool, error) {
	var swapped bool
	if bytes.Compare(aPub[:], bPub[:]) == -1 { // swap to sort pubkeys if needed
		aPub, bPub = bPub, aPub
		swapped = true
	}
	bldr := txscript.NewScriptBuilder()
	// Require 1 signatures, either key// so from both of the pubkeys
	bldr.AddOp(txscript.OP_2)
	// add both pubkeys (sorted)
	bldr.AddData(aPub[:])
	bldr.AddData(bPub[:])
	// 2 keys total.  In case that wasn't obvious.
	bldr.AddOp(txscript.OP_2)
	// Good ol OP_CHECKMULTISIG.  Don't forget the zero!
	bldr.AddOp(txscript.OP_CHECKMULTISIG)
	// get byte slice
	pre, err := bldr.Script()
	return pre, swapped, err
}

// the scriptsig to put on a P2SH input.  Sigs need to be in order!
func SpendMultiSigWitStack(pre, sigA, sigB []byte) [][]byte {

	witStack := make([][]byte, 4)

	witStack[0] = nil // it's not an OP_0 !!!! argh!
	witStack[1] = sigA
	witStack[2] = sigB
	witStack[3] = pre

	return witStack
}

func P2WSHify(scriptBytes []byte) []byte {
	bldr := txscript.NewScriptBuilder()
	bldr.AddOp(txscript.OP_0)
	wsh := fastsha256.Sum256(scriptBytes)
	bldr.AddData(wsh[:])
	b, _ := bldr.Script() // ignore script errors
	return b
}