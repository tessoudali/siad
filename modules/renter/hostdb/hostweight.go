package hostdb

import (
	"math"
	"math/big"

	"gitlab.com/NebulousLabs/Sia/build"
	"gitlab.com/NebulousLabs/Sia/modules"
	"gitlab.com/NebulousLabs/Sia/modules/renter/hostdb/hosttree"
	"gitlab.com/NebulousLabs/Sia/types"
)

var (
	// Because most weights would otherwise be fractional, we set the base
	// weight to be very large.
	baseWeight = types.NewCurrency(new(big.Int).Exp(big.NewInt(10), big.NewInt(80), nil))

	// collateralExponentiation is the power to which we raise the weight
	// during collateral adjustment when the collateral is large. This sublinear
	// number ensures that there is not an overpreference on collateral when
	// collateral is large relative to the size of the allowance.
	collateralExponentiationLarge = 0.5

	// collateralExponentiationSmall is the power to which we raise the weight
	// during collateral adjustment when the collateral is small. This large
	// number ensures a heavy focus on collateral when distinguishing between
	// hosts that have a very small amount of collateral provided compared to
	// the size of the allowance.
	//
	// The number is set relative to the price exponentiation, because the goal
	// is to ensure that the collateral has more weight than the price when the
	// collateral is small.
	collateralExponentiationSmall = priceExponentiationLarge + 1

	// priceDiveNormalization reduces the raw value of the price so that not so
	// many digits are needed when operating on the weight. This also allows the
	// base weight to be a lot lower.
	priceDivNormalization = types.SiacoinPrecision.Div64(100e3).Div64(tbMonth)

	// priceExponentiationLarge is the number of times that the weight is
	// divided by the price when the price is large relative to the allowance.
	// The exponentiation is a lot higher because we care greatly about high
	// priced hosts.
	priceExponentiationLarge = 5.0

	// priceExponentiationSmall is the number of times that the weight is
	// divided by the price when the price is small relative to the allowance.
	// The exponentiation is lower because we do not care about saving
	// substantial amounts of money when the price is low.
	priceExponentiationSmall = 1.5

	// requiredStorage indicates the amount of storage that the host must be
	// offering in order to be considered a valuable/worthwhile host.
	requiredStorage = build.Select(build.Var{
		Standard: uint64(20e9),
		Dev:      uint64(1e6),
		Testing:  uint64(1e3),
	}).(uint64)

	// tbMonth is the number of bytes in a terabyte times the number of blocks
	// in a month.
	tbMonth = uint64(4032) * uint64(1e12)
)

// TODO: These values should be rolled into the allowance, instead of being a
// separate struct that we pass in.
//
// expectedStorage is the amount of data that we expect to have in a contract.
//
// expectedUploadFrequency is the expected number of blocks between each
// complete re-upload of the filesystem. This will be a combination of the rate
// at which a user uploads files, the rate at which a user replaces files, and
// the rate at which a user has to repair files due to host churn. If the
// expected storage is 25 GB and the expected upload frequency is 24 weeks, it
// means the user is expected to do about 1 GB of upload per week on average
// throughout the life of the contract.
//
// expectedDownloadFrequency is the expected number of blocks between each
// complete download of the filesystem. This should include the user
// downloading, streaming, and repairing files.
//
// expectedDataPieces and expectedParityPieces are used to give information
// about the redundancy of the files being uploaded.
type usageGuidelines struct {
	expectedStorage           uint64
	expectedUploadFrequency   uint64
	expectedDownloadFrequency uint64
	expectedDataPieces        uint64
	expectedParityPieces      uint64
}

// collateralAdjustments improves the host's weight according to the amount of
// collateral that they have provided.
func (hdb *HostDB) collateralAdjustments(entry modules.HostDBEntry, allowance modules.Allowance, ug usageGuidelines) float64 {
	// Ensure that all values will avoid divide by zero errors.
	if allowance.Hosts == 0 {
		allowance.Hosts = 1
	}
	if allowance.Period == 0 {
		allowance.Period = 1
	}
	if ug.expectedStorage == 0 {
		ug.expectedStorage = 1
	}
	if ug.expectedUploadFrequency == 0 {
		ug.expectedUploadFrequency = 1
	}
	if ug.expectedDownloadFrequency == 0 {
		ug.expectedDownloadFrequency = 1
	}
	if ug.expectedDataPieces == 0 {
		ug.expectedDataPieces = 1
	}
	if ug.expectedParityPieces == 0 {
		ug.expectedParityPieces = 1
	}

	// Ensure that the allowance and expected storage will not brush up against
	// the max collateral. If the allowance comes within half of the max
	// collateral, cap the collateral that we use during adjustments based on
	// the max collateral instead of the per-byte collateral.
	hostCollateral := entry.Collateral
	possibleCollateral := entry.MaxCollateral.Div64(uint64(allowance.Period)).Div64(ug.expectedStorage).Div64(2)
	if hostCollateral.Cmp(possibleCollateral) < 0 {
		hostCollateral = possibleCollateral
	}

	// Determine the cutoff for the difference between small collateral and
	// large collateral. The cutoff is used to create a step function in the
	// collateral scoring where decreasing collateral results in much higher
	// penalties below a certain threshold.
	//
	// This threshold is attempting to be the threshold where the amount of
	// money becomes insignificant. A collateral that is 10x higher than the
	// price is not interesting, compelling, nor a sign of reliability if the
	// price and collateral are both effectively zero.
	//
	// The strategy is to take our total allowance and divide it by the number
	// of hosts, to get an expected allowance per host. We then adjust based on
	// the period, and then adjust by adding in the expected storage, upload and
	// download. We add the three together so that storage heavy allowances will
	// have higher expectations for collateral than bandwidth heavy allowances.
	// Finally, we divide the whole thing by 5 to give some wiggle room to
	// hosts. The large multiplier provided for low collaterals is only intended
	// to discredit hosts that have a meaningless amount of collateral.
	expectedUploadBandwidth := ug.expectedStorage * uint64(allowance.Period) / ug.expectedUploadFrequency
	expectedDownloadBandwidth := ug.expectedStorage * uint64(allowance.Period) / ug.expectedDownloadFrequency * ug.expectedDataPieces / (ug.expectedDataPieces + ug.expectedParityPieces)
	expectedBandwidth := expectedUploadBandwidth + expectedDownloadBandwidth
	cutoff := allowance.Funds.Div64(allowance.Hosts).Div64(uint64(allowance.Period)).Div64(ug.expectedStorage + expectedBandwidth).Div64(5)
	if hostCollateral.Cmp(cutoff) < 0 {
		// Set the cutoff equal to the collateral so that the ratio has a
		// minimum of 1, and also so that the smallWeight is computed based on
		// the actual collateral instead of just the cutoff.
		cutoff = hostCollateral
	}
	// Get the ratio between the cutoff and the actual collateral so we can
	// award the bonus for having a large collateral. Perform normalization
	// before converting to uint64.
	collateral64, _ := hostCollateral.Div(priceDivNormalization).Uint64()
	cutoff64, _ := cutoff.Div(priceDivNormalization).Uint64()
	if cutoff64 == 0 {
		cutoff64 = 1
	}
	ratio := float64(collateral64) / float64(cutoff64)

	// Use the cutoff to determine the score based on the small exponentiation
	// factor (which has a high exponentiation), and then use the ratio between
	// the two to determine the bonus gained from having a high collateral.
	smallWeight := math.Pow(float64(cutoff64), collateralExponentiationSmall)
	largeWeight := math.Pow(ratio, collateralExponentiationLarge)
	return smallWeight * largeWeight
}

// expectedStorage is the amount of data that we expect to have in a
// contract.
//
// expectedUploadFrequency is the expected number of blocks between each
// complete re-upload of the filesystem. This will be a combination of the
// rate at which a user uploads files, the rate at which a user replaces
// files, and the rate at which a user has to repair files due to host
// churn. If the expected storage is 25 GB and the expected upload frequency
// is 24 weeks, it means the user is expected to do about 1 GB of upload per
// week on average throughout the life of the contract.
//
// expectedDownloadFrequency is the expected number of blocks between each
// complete download of the filesystem. This should include the user
// downloading, streaming, and repairing files.

// interactionAdjustments determine the penalty to be applied to a host for the
// historic and currnet interactions with that host. This function focuses on
// historic interactions and ignores recent interactions.
func (hdb *HostDB) interactionAdjustments(entry modules.HostDBEntry) float64 {
	// Give the host a baseline of 30 successful interactions and 1 failed
	// interaction. This gives the host a baseline if we've had few
	// interactions with them. The 1 failed interaction will become
	// irrelevant after sufficient interactions with the host.
	hsi := entry.HistoricSuccessfulInteractions + 30
	hfi := entry.HistoricFailedInteractions + 1

	// Determine the intraction ratio based off of the historic interactions.
	ratio := float64(hsi) / float64(hsi+hfi)

	// Raise the ratio to the 15th power and return that. The exponentiation is
	// very high because the renter will already intentionally avoid hosts that
	// do not have many successful interactions, meaning that the bad points do
	// not rack up very quickly. We want to signal a bad score for the host
	// nonetheless.
	return math.Pow(ratio, 15)
}

// priceAdjustments will adjust the weight of the entry according to the prices
// that it has set.
func (hdb *HostDB) priceAdjustments(entry modules.HostDBEntry, allowance modules.Allowance, ug usageGuidelines) float64 {
	// Divide by zero mitigation.
	if allowance.Hosts == 0 {
		allowance.Hosts = 1
	}
	if allowance.Period == 0 {
		allowance.Period = 1
	}
	if ug.expectedStorage == 0 {
		ug.expectedStorage = 1
	}
	if ug.expectedUploadFrequency == 0 {
		ug.expectedUploadFrequency = 1
	}
	if ug.expectedDownloadFrequency == 0 {
		ug.expectedDownloadFrequency = 1
	}
	if ug.expectedDataPieces == 0 {
		ug.expectedDataPieces = 1
	}
	if ug.expectedParityPieces == 0 {
		ug.expectedParityPieces = 1
	}

	// Prices tiered as follows:
	//    - the storage price is presented as 'per block per byte'
	//    - the contract price is presented as a flat rate
	//    - the upload bandwidth price is per byte
	//    - the download bandwidth price is per byte
	//
	// The adjusted prices take the pricing for other parts of the contract
	// (like bandwidth and fees) and convert them into terms that are relative
	// to the storage price.
	adjustedContractPrice := entry.ContractPrice.Div64(uint64(allowance.Period)).Div64(ug.expectedStorage)
	adjustedUploadPrice := entry.UploadBandwidthPrice.Div64(ug.expectedUploadFrequency)
	adjustedDownloadPrice := entry.DownloadBandwidthPrice.Div64(ug.expectedDownloadFrequency).Mul64(ug.expectedDataPieces).Div64(ug.expectedDataPieces + ug.expectedParityPieces)
	siafundFee := adjustedContractPrice.Add(adjustedUploadPrice).Add(adjustedDownloadPrice).Add(entry.Collateral).MulTax()
	totalPrice := entry.StoragePrice.Add(adjustedContractPrice).Add(adjustedUploadPrice).Add(adjustedDownloadPrice).Add(siafundFee)

	// Determine a cutoff for whether the total price is considered a high price
	// or a low price. This cutoff attempts to determine where the price becomes
	// insignificant.
	expectedUploadBandwidth := ug.expectedStorage * uint64(allowance.Period) / ug.expectedUploadFrequency
	expectedDownloadBandwidth := ug.expectedStorage * uint64(allowance.Period) / ug.expectedDownloadFrequency * ug.expectedDataPieces / (ug.expectedDataPieces + ug.expectedParityPieces)
	expectedBandwidth := expectedUploadBandwidth + expectedDownloadBandwidth
	cutoff := allowance.Funds.Div64(allowance.Hosts).Div64(uint64(allowance.Period)).Div64(ug.expectedStorage + expectedBandwidth).Div64(5)
	if totalPrice.Cmp(cutoff) < 0 {
		cutoff = totalPrice
	}
	price64, _ := totalPrice.Div(priceDivNormalization).Uint64()
	cutoff64, _ := cutoff.Div(priceDivNormalization).Uint64()
	if cutoff64 == 0 {
		cutoff64 = 1
	}
	ratio := float64(price64) / float64(cutoff64)

	smallWeight := math.Pow(float64(cutoff64), priceExponentiationSmall)
	largeWeight := math.Pow(ratio, priceExponentiationLarge)
	return 1 / (smallWeight * largeWeight)
}

// storageRemainingAdjustments adjusts the weight of the entry according to how
// much storage it has remaining.
func storageRemainingAdjustments(entry modules.HostDBEntry) float64 {
	base := float64(1)
	if entry.RemainingStorage < 200*requiredStorage {
		base = base / 2 // 2x total penalty
	}
	if entry.RemainingStorage < 150*requiredStorage {
		base = base / 2 // 4x total penalty
	}
	if entry.RemainingStorage < 100*requiredStorage {
		base = base / 2 // 8x total penalty
	}
	if entry.RemainingStorage < 80*requiredStorage {
		base = base / 2 // 16x total penalty
	}
	if entry.RemainingStorage < 40*requiredStorage {
		base = base / 2 // 32x total penalty
	}
	if entry.RemainingStorage < 20*requiredStorage {
		base = base / 2 // 64x total penalty
	}
	if entry.RemainingStorage < 15*requiredStorage {
		base = base / 2 // 128x total penalty
	}
	if entry.RemainingStorage < 10*requiredStorage {
		base = base / 2 // 256x total penalty
	}
	if entry.RemainingStorage < 5*requiredStorage {
		base = base / 2 // 512x total penalty
	}
	if entry.RemainingStorage < 3*requiredStorage {
		base = base / 2 // 1024x total penalty
	}
	if entry.RemainingStorage < 2*requiredStorage {
		base = base / 2 // 2048x total penalty
	}
	if entry.RemainingStorage < requiredStorage {
		base = base / 2 // 4096x total penalty
	}
	return base
}

// versionAdjustments will adjust the weight of the entry according to the siad
// version reported by the host.
func versionAdjustments(entry modules.HostDBEntry) float64 {
	base := float64(1)
	if build.VersionCmp(entry.Version, "1.4.0") < 0 {
		base = base * 0.99999 // Safety value to make sure we update the version penalties every time we update the host.
	}
	// -10% for being below 1.3.5.
	if build.VersionCmp(entry.Version, "1.3.5") < 0 {
		base = base * 0.9
	}
	// -10% for being below 1.3.4.
	if build.VersionCmp(entry.Version, "1.3.4") < 0 {
		base = base * 0.9
	}
	// -10% for being below 1.3.3.
	if build.VersionCmp(entry.Version, "1.3.3") < 0 {
		base = base * 0.9
	}
	// we shouldn't use pre hardfork hosts
	if build.VersionCmp(entry.Version, "1.3.1") < 0 {
		base = math.SmallestNonzeroFloat64
	}
	return base
}

// lifetimeAdjustments will adjust the weight of the host according to the total
// amount of time that has passed since the host's original announcement.
func (hdb *HostDB) lifetimeAdjustments(entry modules.HostDBEntry) float64 {
	base := float64(1)
	if hdb.blockHeight >= entry.FirstSeen {
		age := hdb.blockHeight - entry.FirstSeen
		if age < 12000 {
			base = base * 2 / 3 // 1.5x total
		}
		if age < 6000 {
			base = base / 2 // 3x total
		}
		if age < 4000 {
			base = base / 2 // 6x total
		}
		if age < 2000 {
			base = base / 2 // 12x total
		}
		if age < 1000 {
			base = base / 3 // 36x total
		}
		if age < 576 {
			base = base / 3 // 108x total
		}
		if age < 288 {
			base = base / 3 // 324x total
		}
		if age < 144 {
			base = base / 3 // 972x total
		}
	}
	return base
}

// uptimeAdjustments penalizes the host for having poor uptime, and for being
// offline.
//
// CAUTION: The function 'updateEntry' will manually fill out two scans for a
// new host to give the host some initial uptime or downtime. Modification of
// this function needs to be made paying attention to the structure of that
// function.
func (hdb *HostDB) uptimeAdjustments(entry modules.HostDBEntry) float64 {
	// Special case: if we have scanned the host twice or fewer, don't perform
	// uptime math.
	if len(entry.ScanHistory) == 0 {
		return 0.25
	}
	if len(entry.ScanHistory) == 1 {
		if entry.ScanHistory[0].Success {
			return 0.75
		}
		return 0.25
	}
	if len(entry.ScanHistory) == 2 {
		if entry.ScanHistory[0].Success && entry.ScanHistory[1].Success {
			return 0.85
		}
		if entry.ScanHistory[0].Success || entry.ScanHistory[1].Success {
			return 0.50
		}
		return 0.05
	}

	// Compute the total measured uptime and total measured downtime for this
	// host.
	downtime := entry.HistoricDowntime
	uptime := entry.HistoricUptime
	recentTime := entry.ScanHistory[0].Timestamp
	recentSuccess := entry.ScanHistory[0].Success
	for _, scan := range entry.ScanHistory[1:] {
		if recentTime.After(scan.Timestamp) {
			if build.DEBUG {
				hdb.log.Critical("Host entry scan history not sorted.")
			} else {
				hdb.log.Print("WARNING: Host entry scan history not sorted.")
			}
			// Ignore the unsorted scan entry.
			continue
		}
		if recentSuccess {
			uptime += scan.Timestamp.Sub(recentTime)
		} else {
			downtime += scan.Timestamp.Sub(recentTime)
		}
		recentTime = scan.Timestamp
		recentSuccess = scan.Success
	}
	// Sanity check against 0 total time.
	if uptime == 0 && downtime == 0 {
		return 0.001 // Shouldn't happen.
	}

	// Compute the uptime ratio, but shift by 0.02 to acknowledge fully that
	// 98% uptime and 100% uptime is valued the same.
	uptimeRatio := float64(uptime) / float64(uptime+downtime)
	if uptimeRatio > 0.98 {
		uptimeRatio = 0.98
	}
	uptimeRatio += 0.02

	// Cap the total amount of downtime allowed based on the total number of
	// scans that have happened.
	allowedDowntime := 0.03 * float64(len(entry.ScanHistory))
	if uptimeRatio < 1-allowedDowntime {
		uptimeRatio = 1 - allowedDowntime
	}

	// Calculate the penalty for low uptime. Penalties increase extremely
	// quickly as uptime falls away from 95%.
	//
	// 100% uptime = 1
	// 98%  uptime = 1
	// 95%  uptime = 0.83
	// 90%  uptime = 0.26
	// 85%  uptime = 0.03
	// 80%  uptime = 0.001
	// 75%  uptime = 0.00001
	// 70%  uptime = 0.0000001
	exp := 200 * math.Min(1-uptimeRatio, 0.30)
	return math.Pow(uptimeRatio, exp)
}

// calculateHostWeightFn creates a hosttree.WeightFunc given an Allowance.
func (hdb *HostDB) calculateHostWeightFn(allowance modules.Allowance) hosttree.WeightFunc {
	// TODO: Pass these in as input instead of fixing them.
	ug := usageGuidelines{
		expectedStorage:           25e9,
		expectedUploadFrequency:   24192,
		expectedDownloadFrequency: 12096,
		expectedDataPieces:        10,
		expectedParityPieces:      20,
	}

	return func(entry modules.HostDBEntry) types.Currency {
		collateralReward := hdb.collateralAdjustments(entry, allowance, ug)
		interactionPenalty := hdb.interactionAdjustments(entry)
		lifetimePenalty := hdb.lifetimeAdjustments(entry)
		pricePenalty := hdb.priceAdjustments(entry, allowance, ug)
		storageRemainingPenalty := storageRemainingAdjustments(entry)
		uptimePenalty := hdb.uptimeAdjustments(entry)
		versionPenalty := versionAdjustments(entry)

		// Combine the adjustments.
		fullPenalty := collateralReward * interactionPenalty * lifetimePenalty *
			pricePenalty * storageRemainingPenalty * uptimePenalty * versionPenalty

		// Return a types.Currency.
		weight := baseWeight.MulFloat(fullPenalty)
		if weight.IsZero() {
			// A weight of zero is problematic for for the host tree.
			return types.NewCurrency64(1)
		}
		return weight
	}
}

// calculateConversionRate calculates the conversion rate of the provided
// host score, comparing it to the hosts in the database and returning what
// percentage of contracts it is likely to participate in.
func (hdb *HostDB) calculateConversionRate(score types.Currency) float64 {
	var totalScore types.Currency
	for _, h := range hdb.ActiveHosts() {
		totalScore = totalScore.Add(hdb.weightFunc(h))
	}
	if totalScore.IsZero() {
		totalScore = types.NewCurrency64(1)
	}
	conversionRate, _ := big.NewRat(0, 1).SetFrac(score.Mul64(50).Big(), totalScore.Big()).Float64()
	if conversionRate > 100 {
		conversionRate = 100
	}
	return conversionRate
}

// EstimateHostScore takes a HostExternalSettings and returns the estimated
// score of that host in the hostdb, assuming no penalties for age or uptime.
func (hdb *HostDB) EstimateHostScore(entry modules.HostDBEntry, allowance modules.Allowance) modules.HostScoreBreakdown {
	// TODO: Pass these in as input instead of fixing them.
	ug := usageGuidelines{
		expectedStorage:           25e9,
		expectedUploadFrequency:   24192,
		expectedDownloadFrequency: 12096,
		expectedDataPieces:        10,
		expectedParityPieces:      20,
	}

	// Grab the adjustments. Age, and uptime penalties are set to '1', to
	// assume best behavior from the host.
	collateralReward := hdb.collateralAdjustments(entry, allowance, ug)
	pricePenalty := hdb.priceAdjustments(entry, allowance, ug)
	storageRemainingPenalty := storageRemainingAdjustments(entry)
	versionPenalty := versionAdjustments(entry)

	// Combine into a full penalty, then determine the resulting estimated
	// score.
	fullPenalty := collateralReward * pricePenalty * storageRemainingPenalty * versionPenalty
	estimatedScore := baseWeight.MulFloat(fullPenalty)
	if estimatedScore.IsZero() {
		estimatedScore = types.NewCurrency64(1)
	}

	// Compile the estimates into a host score breakdown.
	return modules.HostScoreBreakdown{
		Score:          estimatedScore,
		ConversionRate: hdb.calculateConversionRate(estimatedScore),

		AgeAdjustment:              1,
		BurnAdjustment:             1,
		CollateralAdjustment:       collateralReward,
		PriceAdjustment:            pricePenalty,
		StorageRemainingAdjustment: storageRemainingPenalty,
		UptimeAdjustment:           1,
		VersionAdjustment:          versionPenalty,
	}
}

// ScoreBreakdown provdes a detailed set of scalars and bools indicating
// elements of the host's overall score.
func (hdb *HostDB) ScoreBreakdown(entry modules.HostDBEntry) modules.HostScoreBreakdown {
	// TODO: Pass these in as input instead of fixing them.
	ug := usageGuidelines{
		expectedStorage:           25e9,
		expectedUploadFrequency:   24192,
		expectedDownloadFrequency: 12096,
		expectedDataPieces:        10,
		expectedParityPieces:      20,
	}

	hdb.mu.Lock()
	defer hdb.mu.Unlock()

	score := hdb.weightFunc(entry)
	return modules.HostScoreBreakdown{
		Score:          score,
		ConversionRate: hdb.calculateConversionRate(score),

		AgeAdjustment:              hdb.lifetimeAdjustments(entry),
		BurnAdjustment:             1,
		CollateralAdjustment:       hdb.collateralAdjustments(entry, hdb.allowance, ug),
		InteractionAdjustment:      hdb.interactionAdjustments(entry),
		PriceAdjustment:            hdb.priceAdjustments(entry, hdb.allowance, ug),
		StorageRemainingAdjustment: storageRemainingAdjustments(entry),
		UptimeAdjustment:           hdb.uptimeAdjustments(entry),
		VersionAdjustment:          versionAdjustments(entry),
	}
}
