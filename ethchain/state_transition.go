package ethchain

import (
	"bytes"
	"fmt"
	"github.com/ethereum/eth-go/ethutil"
	"math/big"
)

/*
 * The State transitioning model
 *
 * A state transition is a change made when a transaction is applied to the current world state
 * The state transitioning model does all all the necessary work to work out a valid new state root.
 * 1) Nonce handling
 * 2) Pre pay / buy gas of the coinbase (miner)
 * 3) Create a new state object if the recipient is \0*32
 * 4) Value transfer
 * == If contract creation ==
 * 4a) Attempt to run transaction data
 * 4b) If valid, use result as code for the new state object
 * == end ==
 * 5) Run Script section
 * 6) Derive new state root
 */
type StateTransition struct {
	coinbase, receiver []byte
	tx                 *Transaction
	gas, gasPrice      *big.Int
	value              *big.Int
	data               []byte
	state              *State
	block              *Block

	cb, rec, sen *StateObject
}

func NewStateTransition(coinbase *StateObject, tx *Transaction, state *State, block *Block) *StateTransition {
	return &StateTransition{coinbase.Address(), tx.Recipient, tx, new(big.Int), new(big.Int).Set(tx.GasPrice), tx.Value, tx.Data, state, block, coinbase, nil, nil}
}

func (self *StateTransition) Coinbase() *StateObject {
	if self.cb != nil {
		return self.cb
	}

	self.cb = self.state.GetAccount(self.coinbase)
	return self.cb
}
func (self *StateTransition) Sender() *StateObject {
	if self.sen != nil {
		return self.sen
	}

	self.sen = self.state.GetAccount(self.tx.Sender())
	return self.sen
}
func (self *StateTransition) Receiver() *StateObject {
	if self.tx != nil && self.tx.CreatesContract() {
		return nil
	}

	if self.rec != nil {
		return self.rec
	}

	self.rec = self.state.GetAccount(self.tx.Recipient)
	return self.rec
}

func (self *StateTransition) MakeStateObject(state *State, tx *Transaction) *StateObject {
	contract := MakeContract(tx, state)

	return contract
}

func (self *StateTransition) UseGas(amount *big.Int) error {
	if self.gas.Cmp(amount) < 0 {
		return OutOfGasError()
	}
	self.gas.Sub(self.gas, amount)

	return nil
}

func (self *StateTransition) AddGas(amount *big.Int) {
	self.gas.Add(self.gas, amount)
}

func (self *StateTransition) BuyGas() error {
	var err error

	sender := self.Sender()
	if sender.Amount.Cmp(self.tx.GasValue()) < 0 {
		return fmt.Errorf("Insufficient funds to pre-pay gas. Req %v, has %v", self.tx.GasValue(), sender.Amount)
	}

	coinbase := self.Coinbase()
	err = coinbase.BuyGas(self.tx.Gas, self.tx.GasPrice)
	if err != nil {
		return err
	}

	self.AddGas(self.tx.Gas)
	sender.SubAmount(self.tx.GasValue())

	return nil
}

func (self *StateTransition) RefundGas() {
	coinbase, sender := self.Coinbase(), self.Sender()
	coinbase.RefundGas(self.gas, self.tx.GasPrice)

	// Return remaining gas
	remaining := new(big.Int).Mul(self.gas, self.tx.GasPrice)
	sender.AddAmount(remaining)
}

func (self *StateTransition) preCheck() (err error) {
	var (
		tx     = self.tx
		sender = self.Sender()
	)

	// Make sure this transaction's nonce is correct
	if sender.Nonce != tx.Nonce {
		return NonceError(tx.Nonce, sender.Nonce)
	}

	// Pre-pay gas / Buy gas of the coinbase account
	if err = self.BuyGas(); err != nil {
		return err
	}

	return nil
}

func (self *StateTransition) TransitionState() (err error) {
	statelogger.Infof("(~) %x\n", self.tx.Hash())

	/*
		defer func() {
			if r := recover(); r != nil {
				logger.Infoln(r)
				err = fmt.Errorf("state transition err %v", r)
			}
		}()
	*/

	// XXX Transactions after this point are considered valid.
	if err = self.preCheck(); err != nil {
		return
	}

	var (
		tx       = self.tx
		sender   = self.Sender()
		receiver *StateObject
	)

	defer self.RefundGas()

	// Increment the nonce for the next transaction
	sender.Nonce += 1

	receiver = self.Receiver()

	// Transaction gas
	if err = self.UseGas(GasTx); err != nil {
		return
	}

	// Pay data gas
	dataPrice := big.NewInt(int64(len(self.data)))
	dataPrice.Mul(dataPrice, GasData)
	if err = self.UseGas(dataPrice); err != nil {
		return
	}

	// If the receiver is nil it's a contract (\0*32).
	if receiver == nil {
		// Create a new state object for the contract
		receiver = self.MakeStateObject(self.state, tx)
		if receiver == nil {
			return fmt.Errorf("Unable to create contract")
		}
	}

	// Transfer value from sender to receiver
	if err = self.transferValue(sender, receiver); err != nil {
		return
	}

	// Process the init code and create 'valid' contract
	if IsContractAddr(self.receiver) {
		// Evaluate the initialization script
		// and use the return value as the
		// script section for the state object.
		self.data = nil

		code, err, deepErr := self.Eval(receiver.Init(), receiver)
		if err != nil || deepErr {
			self.state.ResetStateObject(receiver)

			return fmt.Errorf("Error during init script run %v (deepErr = %v)", err, deepErr)
		}

		receiver.script = code
	} else {
		if len(receiver.Script()) > 0 {
			var deepErr bool
			_, err, deepErr = self.Eval(receiver.Script(), receiver)
			if err != nil {
				self.state.ResetStateObject(receiver)

				return fmt.Errorf("Error during code execution %v (deepErr = %v)", err, deepErr)
			}
		}
	}

	return
}

func (self *StateTransition) transferValue(sender, receiver *StateObject) error {
	if sender.Amount.Cmp(self.value) < 0 {
		return fmt.Errorf("Insufficient funds to transfer value. Req %v, has %v", self.value, sender.Amount)
	}

	// Subtract the amount from the senders account
	sender.SubAmount(self.value)
	// Add the amount to receivers account which should conclude this transaction
	receiver.AddAmount(self.value)

	return nil
}

func (self *StateTransition) Eval(script []byte, context *StateObject) (ret []byte, err error, deepErr bool) {
	var (
		block     = self.block
		initiator = self.Sender()
		state     = self.state
	)

	closure := NewClosure(initiator, context, script, state, self.gas, self.gasPrice)
	vm := NewVm(state, nil, RuntimeVars{
		Origin:      initiator.Address(),
		Block:       block,
		BlockNumber: block.Number,
		PrevHash:    block.PrevHash,
		Coinbase:    block.Coinbase,
		Time:        block.Time,
		Diff:        block.Difficulty,
		Value:       self.value,
	})
	vm.Verbose = true
	ret, _, err = closure.Call(vm, self.data, nil)
	deepErr = vm.err != nil

	/*
		var testAddr = ethutil.FromHex("ec4f34c97e43fbb2816cfd95e388353c7181dab1")
		if bytes.Compare(testAddr, context.Address()) == 0 {
			trie := context.state.trie
			trie.NewIterator().Each(func(key string, v *ethutil.Value) {
				v.Decode()
				fmt.Printf("%x : %x\n", key, v.Str())
			})
			fmt.Println("\n\n")
		}
	*/

	Paranoia := true // TODO Create a flag for this
	if Paranoia {
		var (
			trie  = context.state.trie
			trie2 = ethutil.NewTrie(ethutil.Config.Db, "")
		)

		trie.NewIterator().Each(func(key string, v *ethutil.Value) {
			trie2.Update(key, v.Str())
		})

		a := ethutil.NewValue(trie2.Root).Bytes()
		b := ethutil.NewValue(context.state.trie.Root).Bytes()
		if bytes.Compare(a, b) != 0 {
			/*
				statelogger.Debugf("(o): %x\n", trie.Root)
				trie.NewIterator().Each(func(key string, v *ethutil.Value) {
					v.Decode()
					statelogger.Debugf("%x : %x\n", key, v.Str())
				})

				statelogger.Debugf("(c): %x\n", trie2.Root)
				trie2.NewIterator().Each(func(key string, v *ethutil.Value) {
					v.Decode()
					statelogger.Debugf("%x : %x\n", key, v.Str())
				})
			*/

			return nil, fmt.Errorf("PARANOIA: Different state object roots during copy"), false
		}
	}

	return
}