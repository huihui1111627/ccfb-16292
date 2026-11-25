package pawn

// 属性名集中定义，页面和生成器共用。
const (
	AttrCategory   = "category"
	AttrGoldWeight = "gold_weight"
	AttrAppraisal  = "appraisal"
	AttrBrand      = "brand"
	AttrWatchModel = "watch_model"
	AttrEra        = "era"
	AttrStyle      = "style"
)

var AttrLabels = map[string]string{
	AttrCategory:   "品类",
	AttrGoldWeight: "克重(g)",
	AttrAppraisal:  "估价(元)",
	AttrBrand:      "品牌",
	AttrWatchModel: "型号/款式",
	AttrEra:        "年代",
	AttrStyle:      "风格",
}

var (
	goldCategories = []string{"黄金", "足金", "K金", "铂金"}
	allCategories  = []string{
		"黄金", "足金", "K金", "铂金", "钻石", "名表", "古董", "金饰",
		"玉器", "翡翠", "名牌包", "珠宝", "数码", "名酒",
	}
	brands = []string{
		"Rolex", "Omega", "Cartier", "PatekPhilippe", "AudemarsPiguet",
		"ChowTaiFook", "LaoFengXiang", "Tiffany", "Hermes", "Chanel",
		"LouisVuitton", "Gucci", "Prada", "VacheronConstantin", "IWC",
	}
	watchModels = []string{
		"Submariner", "Datejust", "Daytona", "GMT-Master", "Speedmaster",
		"Seamaster", "Tank", "BallonBleu", "Overseas", "Portugieser",
	}
	styles = []string{"商务", "运动", "复古", "经典", "限量", "女款", "男款"}
	eras   = []string{"清代", "明代", "民国", "现代", "唐宋", "汉代"}

	weightBounds    = []float64{5, 10, 20, 30, 50, 80, 100, 150}
	appraisalBounds = []float64{
		500, 1000, 2000, 5000, 10000, 20000, 50000,
		100000, 200000, 500000, 1000000,
	}
)

type Config struct {
	Seed int64

	StoreCount  int
	RegionCount int

	// InitialPerStore 是第 0 条消息前已存在的订阅数。
	InitialPerStore int
	// ExtraSubs 在消息处理过程中新增；总生成订阅数 = InitialPerStore*StoreCount + ExtraSubs。
	ExtraSubs int
	// DeleteCount 从初始订阅中确定性删除。
	DeleteCount int

	MessageCount int
	SampleSize   int

	// ProgressEvery 为 0 时不输出进度。
	ProgressEvery int
}

func DefaultConfig() Config {
	return Config{
		Seed:            20261126,
		StoreCount:      64,
		RegionCount:     8,
		InitialPerStore: 3750, // 64 * 3750 = 240000 条初始订阅
		ExtraSubs:       6400,
		DeleteCount:     6400,
		MessageCount:    2_400_000,
		SampleSize:      128,
		ProgressEvery:   200_000,
	}
}

func SmallConfig() Config {
	return Config{
		Seed:            42,
		StoreCount:      12,
		RegionCount:     3,
		InitialPerStore: 120,
		ExtraSubs:       180,
		DeleteCount:     60,
		MessageCount:    20_000,
		SampleSize:      128,
	}
}
