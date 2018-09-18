const maxUserNumber = 10;

const iost = 100000000;

class Contract {
    init() {
        storage.put("user_number", JSON.stringify(0));
        storage.put("total_coins", JSON.stringify(0));
        storage.put("last_lucky_block", JSON.stringify(-1));
        storage.put("round", JSON.stringify(1));

        this.clearUserValue()
    }
    clearUserValue() {
        for (let i = 0; i < 10; i ++) {
            storage.mapPut('table', i.toString(), JSON.stringify([]))
        }
    }
    bet(account, luckyNumber, coins, nonce) {
        if (coins < iost || coins > 5 * iost) {
            return "bet coins should be >=1 iost and <= 5 iost"
        }
        if (luckyNumber < 0 && luckyNumber > 9) {
            return "bet coins should be >=1 and <= 5"
        }

        BlockChain.deposit(account, coins);

        const userNumber = JSON.parse(storage.get("user_number"));
        const totalCoins = JSON.parse(storage.get("total_coins"));

        let table = JSON.parse(storage.mapGet('table', luckyNumber.toString()));

        table.push({ account:account, coins : coins, nonce : nonce});

        storage.mapPut('table', luckyNumber.toString(), JSON.stringify(table));

        if (userNumber + 1 >= maxUserNumber) {
            const bi = JSON.parse(BlockChain.blockInfo());
            const bn = bi.number;
            const ph = bi.parent_hash;
            const lastLuckyBlock = JSON.parse(storage.get("last_lucky_block"));

            if ( lastLuckyBlock < 0 || bn - lastLuckyBlock >= 16 || bn > lastLuckyBlock && ph[ph.length-1] % 16 === 0) {
                storage.put("last_lucky_block", JSON.stringify(bn));
                const round = JSON.parse(storage.get("round"));

                this.getReward(bn % 10, round);
                storage.put("user_number", JSON.stringify(0));
                storage.put("total_coins", JSON.stringify(0));
                this.clearUserValue();

                storage.put("round", JSON.stringify(round + 1));
                return
            }
        }

        storage.put("user_number", JSON.stringify(userNumber + 1));
        storage.put("total_coins", JSON.stringify(totalCoins + coins));
    }

    getReward(ln, round) {
        const y = new Int64(100);
        const x = new Int64(95);

        const totalCoins = JSON.parse(storage.get("total_coins"));
        const _tc = new Int64(totalCoins);

        const tc = _tc.multi(x).div(y);
        let totalVal = 0;
        let kNum = 0;

        const winTable = JSON.parse(storage.mapGet('table', ln.toString()));

        if (winTable !== undefined && winTable !== null) {
            winTable.forEach(function (record) {
                totalVal += record.coins;
                kNum++;
            });
        }

        let result = {
            number: this.lastLuckyBlock,
            user_number: this.userNumber,
            k_number: kNum,
            total_coins : tc,
            records : []
        };

        for (let i = 0; i < 10; i ++) {
            let table = [];
            if( i === ln) {
                table = winTable;
                table.forEach(function (record) {
                    const reward = (tc.multi(record.coins).div(totalVal));
                    BlockChain.withdraw(record.account, reward);
                    record.reward = reward.toString();
                    result.records.push(record)
                })
            } else {
                table = JSON.parse(storage.mapGet('table', i.toString()));
                table.forEach(function (record) {
                    result.records.push(record)
                })
            }

        }

        storage.put('result'+round.toString(), JSON.stringify(result));
    }
}

module.exports = Contract;