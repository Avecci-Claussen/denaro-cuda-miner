package main

/*
void start(const int device_id, const int threads, const int blocks, unsigned char *prefix, char *share_chunk, int share_difficulty, char *charset, const char *out);

#cgo LDFLAGS: -L. -L./ -lkernel
*/
// #include <stdlib.h>
import "C"

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/btcsuite/btcutil/base58"
	"log"
	"math"
	"os/exec"
	"strings"
	"time"
	"unsafe"
)

const devAddress = "DnAmhfPcckW4yDCVdaMQtPs6CkSfsQNyDrJ6kZzanJpty"

var (
	address string
	nodeUrl string
	poolUrl string

	deviceId int
	threads  int
	blocks   int

	silent bool

	shareDifficulty int
	shares          = 0

	devFee            int // 1 every X shares are sent to the dev
	devFeeMustProcess = false
)

func main() {
	flag.StringVar(&address, "address", "", "denaro address (https://t.me/DenaroCoinBot)")
	flag.StringVar(&nodeUrl, "node", "https://denaro-node.gaetano.eu.org/", "denaro node url")
	flag.StringVar(&poolUrl, "pool", "https://denaro-pool.gaetano.eu.org/", "denaro pool url")

	flag.BoolVar(&silent, "silent", false, "silent mode (no output)")

	flag.IntVar(&deviceId, "device", 0, "gpu device id")
	flag.IntVar(&threads, "threads", 512, "gpu threads")
	flag.IntVar(&blocks, "blocks", 50, "gpu blocks")

	flag.IntVar(&shareDifficulty, "share", 7, "share difficulty")
	flag.IntVar(&devFee, "fee", 5, "dev fee (1 every X shares are sent to the dev)")

	flag.Parse()

	// ask for address if not inserted as flag
	if len(address) == 0 {
		fmt.Print("Insert your address (available at https://t.me/DenaroCoinBot): ")
		if _, err := fmt.Scan(&address); err != nil {
			panic(err)
		}
	}

	var miningAddressT MiningAddress

	miningAddressReq := GET(
		poolUrl+"get_mining_address",
		map[string]interface{}{
			"address": address,
		},
	)
	if err := json.Unmarshal(miningAddressReq.Body(), &miningAddressT); err != nil {
		panic(err)
	}

	for {
		if !silent {
			printUI()
		}

		var reqP MiningInfo
		req := GET(nodeUrl+"get_mining_info", map[string]interface{}{})
		_ = json.Unmarshal(req.Body(), &reqP)

		miner(miningAddressT.Address, reqP.Result)
	}
}

func printUI() {
	deviceName, _ := exec.Command("nvidia-smi", "--query-gpu=name", "--format=csv", "--id="+fmt.Sprint(deviceId)).Output()

	var hashrates Hashrates
	req := GET(poolUrl+"hashrates", map[string]interface{}{})
	_ = json.Unmarshal(req.Body(), &hashrates)

	fmt.Print("\033[H\033[2J")
	fmt.Println("Device: ", strings.Replace(strings.Replace(string(deviceName), "name\n", "", 1), "\n", "", -1))
	fmt.Println("Address: ", address)
	fmt.Println("Hashrate: ", hashrates.Result[address]/1_000_000, "MH/s")
	fmt.Println()
	fmt.Println("Pool: ", poolUrl)
	fmt.Println("Node: ", nodeUrl)
	fmt.Println()
	fmt.Println("Shares: ", shares)
	fmt.Println("Dev fee: 1 share every", devFee, "shares")
	fmt.Println()
	fmt.Println("Last update: ", time.Now().Format("15:04:05"))
}

func miner(miningAddress string, res MiningInfoResult) {
	var difficulty = res.Difficulty
	var idifficulty = int(difficulty)

	defer func() {
		if r := recover(); r != nil {
			log.Printf("Recovered from: %v\n", r)
		}
	}()

	_, decimal := math.Modf(difficulty)

	lastBlock := res.LastBlock
	if lastBlock.Hash == "" {
		var num uint32 = 30_06_2005

		data := make([]byte, 32)
		binary.LittleEndian.PutUint32(data, num)

		lastBlock.Hash = hex.EncodeToString(data)
	}

	chunk := lastBlock.Hash[len(lastBlock.Hash)-idifficulty:]

	var shareChunk string

	if shareDifficulty > idifficulty {
		shareDifficulty = idifficulty
	}
	shareChunk = chunk[:shareDifficulty]

	charset := "0123456789abcdef"
	if decimal > 0 {
		count := math.Ceil(16 * (1 - decimal))
		charset = charset[:int(count)]
	}

	var addressBytes []byte
	if devFeeMustProcess {
		addressBytes = stringToBytes(devAddress)
	} else {
		addressBytes = stringToBytes(miningAddress)
	}
	a := time.Now().Unix()
	txs := res.PendingTransactionsHashes
	merkleTree := getTransactionsMerkleTree(txs)

	var prefix []byte
	// version, not supporting v1
	dataVersion := make([]byte, 2)
	binary.LittleEndian.PutUint16(dataVersion, uint16(2))
	prefix = append(prefix, dataVersion[0])

	dataHash, _ := hex.DecodeString(lastBlock.Hash)
	prefix = append(prefix, dataHash...)
	prefix = append(prefix, addressBytes...)
	dataMerkleTree, _ := hex.DecodeString(merkleTree)
	prefix = append(prefix, dataMerkleTree...)
	dataA := make([]byte, 4)
	binary.LittleEndian.PutUint32(dataA, uint32(a))
	prefix = append(prefix, dataA...)
	dataDifficulty := make([]byte, 2)
	binary.LittleEndian.PutUint16(dataDifficulty, uint16(difficulty*10))
	prefix = append(prefix, dataDifficulty...)

	var result = make([]byte, 108)

	var shareChunkGpu = C.CString(shareChunk)
	var charsetGpu = C.CString(charset)

	C.start(
		C.int(deviceId),
		C.int(threads),
		C.int(blocks),
		(*C.uchar)(unsafe.Pointer(&prefix[0])),
		shareChunkGpu,
		C.int(shareDifficulty),
		charsetGpu,
		(*C.char)(unsafe.Pointer(&result[0])),
	)

	// check if first byte of result is 2, which currently is the version indicator
	if result[0] == 2 {
		var shareT Share

		shareReq := POST(
			poolUrl+"share",
			map[string]interface{}{
				"block_content":    hex.EncodeToString(result),
				"txs":              txs,
				"id":               lastBlock.Id + 1,
				"share_difficulty": difficulty,
			},
		)
		_ = json.Unmarshal(shareReq.Body(), &shareT)

		// process dev fee
		devFeeText := ""
		if devFee > 0 && shares%devFee == 0 {
			devFeeMustProcess = true
		} else if devFeeMustProcess {
			devFeeMustProcess = false
			devFeeText = "(dev fee)"
		}

		if shareT.Ok {
			shares++

			if !silent {
				log.Printf("Share accepted (device: %d) %s\n", deviceId, devFeeText)
				log.Println(hex.EncodeToString(result))
			}
		} else if !silent {
			log.Println(string(shareReq.Body()))
			log.Println(hex.EncodeToString(result))
		}
	}
	C.free(unsafe.Pointer(shareChunkGpu))
	C.free(unsafe.Pointer(charsetGpu))
}

func getTransactionsMerkleTree(transactions []string) string {

	var fullData []byte

	for _, transaction := range transactions {
		data, _ := hex.DecodeString(transaction)
		fullData = append(fullData, data...)
	}

	hash := sha256.New()
	hash.Write(fullData)

	return hex.EncodeToString(hash.Sum(nil))
}

func stringToBytes(text string) []byte {

	var data []byte

	data, err := hex.DecodeString(text)
	if err != nil {
		data = base58.Decode(text)
	}

	return data
}
