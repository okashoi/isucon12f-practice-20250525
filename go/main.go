package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/snowflake"
	_ "github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"github.com/pkg/errors"
)

var (
	ErrInvalidRequestBody       error = fmt.Errorf("invalid request body")
	ErrInvalidMasterVersion     error = fmt.Errorf("invalid master version")
	ErrInvalidItemType          error = fmt.Errorf("invalid item type")
	ErrInvalidToken             error = fmt.Errorf("invalid token")
	ErrGetRequestTime           error = fmt.Errorf("failed to get request time")
	ErrExpiredSession           error = fmt.Errorf("session expired")
	ErrUserNotFound             error = fmt.Errorf("not found user")
	ErrUserDeviceNotFound       error = fmt.Errorf("not found user device")
	ErrItemNotFound             error = fmt.Errorf("not found item")
	ErrLoginBonusRewardNotFound error = fmt.Errorf("not found login bonus reward")
	ErrNoFormFile               error = fmt.Errorf("no such file")
	ErrUnauthorized             error = fmt.Errorf("unauthorized user")
	ErrForbidden                error = fmt.Errorf("forbidden")
	ErrGeneratePassword         error = fmt.Errorf("failed to password hash") //nolint:deadcode

	dbHosts []string = strings.Split(getEnv("ISUCON_DB_HOSTS", "127.0.0.1"), ",")
)

const (
	DeckCardNumber      int = 3
	PresentCountPerPage int = 100

	SQLDirectory string = "../sql/"
)

type Handler struct {
	DBs        []*sqlx.DB
	DB         *sqlx.DB
	Cache      *MasterDataCache
	TokenCache *TokenCache
}

// MasterDataCache マスターデータのキャッシュ
type MasterDataCache struct {
	mu                sync.RWMutex
	gachaItems        map[int64][]*GachaItemMaster
	gachaWeightSums   map[int64]int64
	loginBonusRewards map[string]*LoginBonusRewardMaster
	itemMasters       map[int64]*ItemMaster
	lastUpdated       time.Time
	masterVersion     string
}

// TokenCache ワンタイムトークンのキャッシュ
type TokenCache struct {
	mu     sync.RWMutex
	tokens map[string]*TokenInfo
}

// TokenInfo トークン情報
type TokenInfo struct {
	UserID    int64
	TokenType int
	ExpiredAt int64
	CreatedAt int64
}

// NewMasterDataCache 新しいキャッシュインスタンスを作成
func NewMasterDataCache() *MasterDataCache {
	return &MasterDataCache{
		gachaItems:        make(map[int64][]*GachaItemMaster),
		gachaWeightSums:   make(map[int64]int64),
		loginBonusRewards: make(map[string]*LoginBonusRewardMaster),
		itemMasters:       make(map[int64]*ItemMaster),
	}
}

// NewTokenCache 新しいトークンキャッシュインスタンスを作成
func NewTokenCache() *TokenCache {
	return &TokenCache{
		tokens: make(map[string]*TokenInfo),
	}
}

// SetToken トークンをキャッシュに設定
func (tc *TokenCache) SetToken(token string, userID int64, tokenType int, expiredAt int64, createdAt int64) {
	tc.mu.Lock()
	defer tc.mu.Unlock()

	tc.tokens[token] = &TokenInfo{
		UserID:    userID,
		TokenType: tokenType,
		ExpiredAt: expiredAt,
		CreatedAt: createdAt,
	}
}

// GetToken トークンをキャッシュから取得
func (tc *TokenCache) GetToken(token string) (*TokenInfo, bool) {
	tc.mu.RLock()
	defer tc.mu.RUnlock()

	tokenInfo, exists := tc.tokens[token]
	return tokenInfo, exists
}

// DeleteToken トークンをキャッシュから削除
func (tc *TokenCache) DeleteToken(token string) {
	tc.mu.Lock()
	defer tc.mu.Unlock()

	delete(tc.tokens, token)
}

// CleanupExpiredTokens 期限切れトークンをクリーンアップ
func (tc *TokenCache) CleanupExpiredTokens(currentTime int64) {
	tc.mu.Lock()
	defer tc.mu.Unlock()

	for token, info := range tc.tokens {
		if info.ExpiredAt < currentTime {
			delete(tc.tokens, token)
		}
	}
}

// GetGachaItems ガチャアイテムをキャッシュから取得
func (c *MasterDataCache) GetGachaItems(gachaID int64) ([]*GachaItemMaster, int64, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	items, exists := c.gachaItems[gachaID]
	if !exists {
		return nil, 0, false
	}

	weightSum, exists := c.gachaWeightSums[gachaID]
	if !exists {
		return nil, 0, false
	}

	return items, weightSum, true
}

// SetGachaItems ガチャアイテムをキャッシュに設定
func (c *MasterDataCache) SetGachaItems(gachaID int64, items []*GachaItemMaster) {
	c.mu.Lock()
	defer c.mu.Unlock()

	var weightSum int64
	for _, item := range items {
		weightSum += int64(item.Weight)
	}

	c.gachaItems[gachaID] = items
	c.gachaWeightSums[gachaID] = weightSum
}

// GetLoginBonusReward ログインボーナス報酬をキャッシュから取得
func (c *MasterDataCache) GetLoginBonusReward(loginBonusID int64, sequence int) (*LoginBonusRewardMaster, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	key := fmt.Sprintf("%d_%d", loginBonusID, sequence)
	reward, exists := c.loginBonusRewards[key]
	return reward, exists
}

// SetLoginBonusReward ログインボーナス報酬をキャッシュに設定
func (c *MasterDataCache) SetLoginBonusReward(reward *LoginBonusRewardMaster) {
	c.mu.Lock()
	defer c.mu.Unlock()

	key := fmt.Sprintf("%d_%d", reward.LoginBonusID, reward.RewardSequence)
	c.loginBonusRewards[key] = reward
}

// GetItemMaster アイテムマスターをキャッシュから取得
func (c *MasterDataCache) GetItemMaster(itemID int64) (*ItemMaster, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	item, exists := c.itemMasters[itemID]
	return item, exists
}

// SetItemMaster アイテムマスターをキャッシュに設定
func (c *MasterDataCache) SetItemMaster(item *ItemMaster) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.itemMasters[item.ID] = item
}

// Clear キャッシュをクリア
func (c *MasterDataCache) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.gachaItems = make(map[int64][]*GachaItemMaster)
	c.gachaWeightSums = make(map[int64]int64)
	c.loginBonusRewards = make(map[string]*LoginBonusRewardMaster)
	c.itemMasters = make(map[int64]*ItemMaster)
	c.lastUpdated = time.Time{}
	c.masterVersion = ""
}

var (
	snowflakeNode *snowflake.Node
)

func main() {
	rand.Seed(time.Now().UnixNano())
	time.Local = time.FixedZone("Local", 9*60*60)

	node, err := snowflake.NewNode(1)
	if err != nil {
		fmt.Println(err)
		return
	}
	snowflakeNode = node

	e := echo.New()
	e.Use(middleware.Logger())
	e.Use(middleware.Recover())
	e.Use(middleware.CORSWithConfig(middleware.CORSConfig{
		AllowOrigins: []string{"*"},
		AllowMethods: []string{http.MethodGet, http.MethodPost},
		AllowHeaders: []string{"Content-Type", "x-master-version", "x-session"},
	}))

	dbx, err := connectDB(false)
	if err != nil {
		e.Logger.Fatalf("failed to connect to db: %v", err)
	}
	defer dbx.Close()

	// Connect to multiple databases for sharding
	dbs, err := connectDBs(false)
	if err != nil {
		e.Logger.Fatalf("failed to connect to dbs: %v", err)
	}
	// Defer closing all database connections
	defer func() {
		for _, db := range dbs {
			db.Close()
		}
	}()

	e.Server.Addr = fmt.Sprintf(":%v", "8080")
	h := &Handler{
		DBs:        dbs,
		DB:         dbx,
		Cache:      NewMasterDataCache(),
		TokenCache: NewTokenCache(),
	}

	e.Use(middleware.CORSWithConfig(middleware.CORSConfig{}))

	// utility
	e.POST("/initialize", initialize)
	e.POST("/initializeOne", initializeOne)
	e.GET("/health", h.health)

	// feature
	API := e.Group("", h.apiMiddleware)
	API.POST("/user", h.createUser)
	API.POST("/login", h.login)
	sessCheckAPI := API.Group("", h.checkSessionMiddleware)
	sessCheckAPI.GET("/user/:userID/gacha/index", h.listGacha)
	sessCheckAPI.POST("/user/:userID/gacha/draw/:gachaID/:n", h.drawGacha)
	sessCheckAPI.GET("/user/:userID/present/index/:n", h.listPresent)
	sessCheckAPI.POST("/user/:userID/present/receive", h.receivePresent)
	sessCheckAPI.GET("/user/:userID/item", h.listItem)
	sessCheckAPI.POST("/user/:userID/card/addexp/:cardID", h.addExpToCard)
	sessCheckAPI.POST("/user/:userID/card", h.updateDeck)
	sessCheckAPI.POST("/user/:userID/reward", h.reward)
	sessCheckAPI.GET("/user/:userID/home", h.home)

	// admin
	adminAPI := e.Group("", h.adminMiddleware)
	adminAPI.POST("/admin/login", h.adminLogin)
	adminAuthAPI := adminAPI.Group("", h.adminSessionCheckMiddleware)
	adminAuthAPI.DELETE("/admin/logout", h.adminLogout)
	adminAuthAPI.GET("/admin/master", h.adminListMaster)
	adminAuthAPI.PUT("/admin/master", h.adminUpdateMaster)
	adminAuthAPI.GET("/admin/user/:userID", h.adminUser)
	adminAuthAPI.POST("/admin/user/:userID/ban", h.adminBanUser)

	e.Logger.Infof("Start server: address=%s", e.Server.Addr)
	e.Logger.Error(e.StartServer(e.Server))
}

// connectDB DBに接続する
func connectDB(batch bool) (*sqlx.DB, error) {
	dsn := fmt.Sprintf(
		"%s:%s@tcp(%s:%s)/%s?charset=utf8mb4&parseTime=true&loc=%s&multiStatements=%t&interpolateParams=true",
		getEnv("ISUCON_DB_USER", "isucon"),
		getEnv("ISUCON_DB_PASSWORD", "isucon"),
		getEnv("ISUCON_DB_HOST", "127.0.0.1"),
		getEnv("ISUCON_DB_PORT", "3306"),
		getEnv("ISUCON_DB_NAME", "isucon"),
		"Asia%2FTokyo",
		batch,
	)
	dbx, err := sqlx.Open("mysql", dsn)
	if err != nil {
		return nil, err
	}

	dbx.SetMaxOpenConns(100)                  // 最大接続数を100に設定
	dbx.SetMaxIdleConns(100)                  // アイドル接続数も100に設定
	dbx.SetConnMaxLifetime(300 * time.Second) // 接続の最大生存時間を5分に設定

	return dbx, nil
}

// connectDBs 複数のDBに接続する
func connectDBs(batch bool) ([]*sqlx.DB, error) {
	hosts := getEnv("ISUCON_DB_HOSTS", "127.0.0.1")
	hostList := strings.Split(hosts, ",")

	dbs := make([]*sqlx.DB, 0, len(hostList))
	for _, host := range hostList {
		dsn := fmt.Sprintf(
			"%s:%s@tcp(%s:%s)/%s?charset=utf8mb4&parseTime=true&loc=%s&multiStatements=%t",
			getEnv("ISUCON_DB_USER", "isucon"),
			getEnv("ISUCON_DB_PASSWORD", "isucon"),
			host,
			getEnv("ISUCON_DB_PORT", "3306"),
			getEnv("ISUCON_DB_NAME", "isucon"),
			"Asia%2FTokyo",
			batch,
		)
		dbx, err := sqlx.Open("mysql", dsn)
		if err != nil {
			// Close all opened connections
			for _, db := range dbs {
				db.Close()
			}
			return nil, err
		}
		dbs = append(dbs, dbx)
	}

	if len(dbs) == 0 {
		// Fallback to single DB connection
		db, err := connectDB(batch)
		if err != nil {
			return nil, err
		}
		dbs = append(dbs, db)
	}

	return dbs, nil
}

// adminMiddleware 管理者ツール向けのmiddleware
func (h *Handler) adminMiddleware(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		requestAt := time.Now()
		c.Set("requestTime", requestAt.Unix())

		// next
		if err := next(c); err != nil {
			c.Error(err)
		}
		return nil
	}
}

// apiMiddleware　ユーザ向けAPI向けのmiddleware
func (h *Handler) apiMiddleware(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		requestAt, err := time.Parse(time.RFC1123, c.Request().Header.Get("x-isu-date"))
		if err != nil {
			requestAt = time.Now()
		}
		c.Set("requestTime", requestAt.Unix())

		// 有効なマスタデータか確認
		query := "SELECT * FROM version_masters WHERE status=1"
		masterVersion := new(VersionMaster)
		if err := h.DB.Get(masterVersion, query); err != nil {
			if err == sql.ErrNoRows {
				return errorResponse(c, http.StatusNotFound, fmt.Errorf("active master version is not found"))
			}
			return errorResponse(c, http.StatusInternalServerError, err)
		}

		if masterVersion.MasterVersion != c.Request().Header.Get("x-master-version") {
			return errorResponse(c, http.StatusUnprocessableEntity, ErrInvalidMasterVersion)
		}

		// BANユーザ確認
		userID, err := getUserID(c)
		if err == nil && userID != 0 {
			isBan, err := h.checkBan(userID)
			if err != nil {
				return errorResponse(c, http.StatusInternalServerError, err)
			}
			if isBan {
				return errorResponse(c, http.StatusForbidden, ErrForbidden)
			}
		}

		if err := next(c); err != nil {
			c.Error(err)
		}
		return nil
	}
}

// checkSessionMiddleware セッションが有効か確認するmiddleware
func (h *Handler) checkSessionMiddleware(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		sessID := c.Request().Header.Get("x-session")
		if sessID == "" {
			return errorResponse(c, http.StatusUnauthorized, ErrUnauthorized)
		}

		userID, err := getUserID(c)
		if err != nil {
			return errorResponse(c, http.StatusBadRequest, err)
		}

		requestAt, err := getRequestTime(c)
		if err != nil {
			return errorResponse(c, http.StatusInternalServerError, ErrGetRequestTime)
		}

		// ユーザーIDに基づいて適切なDBを選択
		db := h.getDBForUserID(userID)

		userSession := new(Session)
		query := "SELECT * FROM user_sessions WHERE session_id=? AND deleted_at IS NULL"
		if err := db.Get(userSession, query, sessID); err != nil {
			if err == sql.ErrNoRows {
				return errorResponse(c, http.StatusUnauthorized, ErrUnauthorized)
			}
			return errorResponse(c, http.StatusInternalServerError, err)
		}

		if userSession.UserID != userID {
			return errorResponse(c, http.StatusForbidden, ErrForbidden)
		}

		// 期限切れチェック
		if userSession.ExpiredAt < requestAt {
			query = "UPDATE user_sessions SET deleted_at=? WHERE session_id=?"
			if _, err = db.Exec(query, requestAt, sessID); err != nil {
				return errorResponse(c, http.StatusInternalServerError, err)
			}
			return errorResponse(c, http.StatusUnauthorized, ErrExpiredSession)
		}

		if err := next(c); err != nil {
			c.Error(err)
		}
		return nil
	}
}

// checkOneTimeToken ワンタイムトークンの確認用middleware
func (h *Handler) checkOneTimeToken(userID int64, token string, tokenType int, requestAt int64) error {
	// まずキャッシュから確認
	if tokenInfo, exists := h.TokenCache.GetToken(token); exists {
		// トークンタイプが一致しない場合
		if tokenInfo.TokenType != tokenType {
			return ErrInvalidToken
		}

		// 期限切れの場合
		if tokenInfo.ExpiredAt < requestAt {
			h.TokenCache.DeleteToken(token)
			// DBからも削除
			query := "UPDATE user_one_time_tokens SET deleted_at=? WHERE token=?"
			h.getDBForUserID(userID).Exec(query, requestAt, token)
			return ErrInvalidToken
		}

		// 使用済みとしてキャッシュから削除
		h.TokenCache.DeleteToken(token)
		// DBからも削除
		query := "UPDATE user_one_time_tokens SET deleted_at=? WHERE token=?"
		if _, err := h.getDBForUserID(userID).Exec(query, requestAt, token); err != nil {
			return err
		}

		return nil
	}

	// キャッシュにない場合はDBから確認（フォールバック）
	tk := new(UserOneTimeToken)
	// ユーザーIDに基づいて適切なDBを選択
	db := h.getDBForUserID(userID)
	query := "SELECT * FROM user_one_time_tokens WHERE token=? AND token_type=? AND deleted_at IS NULL"
	if err := db.Get(tk, query, token, tokenType); err != nil {
		if err == sql.ErrNoRows {
			return ErrInvalidToken
		}
		return err
	}

	if tk.ExpiredAt < requestAt {
		query := "UPDATE user_one_time_tokens SET deleted_at=? WHERE token=?"
		if _, err := db.Exec(query, requestAt, token); err != nil {
			return err
		}
		return ErrInvalidToken
	}

	// 使ったトークンは失効する
	query = "UPDATE user_one_time_tokens SET deleted_at=? WHERE token=?"
	if _, err := db.Exec(query, requestAt, token); err != nil {
		return err
	}

	return nil
}

// checkViewerID viewerIDとplatformの確認を行う
func (h *Handler) checkViewerID(userID int64, viewerID string) error {
	// ユーザーIDに基づいて適切なDBを選択
	db := h.getDBForUserID(userID)

	query := "SELECT * FROM user_devices WHERE user_id=? AND platform_id=?"
	device := new(UserDevice)
	if err := db.Get(device, query, userID, viewerID); err != nil {
		if err == sql.ErrNoRows {
			return ErrUserDeviceNotFound
		}
		return err
	}

	return nil
}

// checkBan BANされているユーザでかを確認する
func (h *Handler) checkBan(userID int64) (bool, error) {
	// ユーザーIDに基づいて適切なDBを選択
	db := h.getDBForUserID(userID)

	banUser := new(UserBan)
	query := "SELECT * FROM user_bans WHERE user_id=?"
	if err := db.Get(banUser, query, userID); err != nil {
		if err == sql.ErrNoRows {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// getRequestTime リクエストを受けた時間をコンテキストからunix timeで取得する
func getRequestTime(c echo.Context) (int64, error) {
	v := c.Get("requestTime")
	if requestTime, ok := v.(int64); ok {
		return requestTime, nil
	}
	return 0, ErrGetRequestTime
}

// loginProcess ログイン処理
func (h *Handler) loginProcess(tx *sqlx.Tx, userID int64, requestAt int64) (*User, []*UserLoginBonus, []*UserPresent, error) {
	user := new(User)
	query := "SELECT * FROM users WHERE id=?"
	if err := tx.Get(user, query, userID); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil, nil, ErrUserNotFound
		}
		return nil, nil, nil, err
	}

	// ログインボーナス処理
	loginBonuses, err := h.obtainLoginBonus(tx, userID, requestAt)
	if err != nil {
		return nil, nil, nil, err
	}

	// 全員プレゼント取得
	allPresents, err := h.obtainPresent(tx, userID, requestAt)
	if err != nil {
		return nil, nil, nil, err
	}

	if err = tx.Get(&user.IsuCoin, "SELECT isu_coin FROM users WHERE id=?", user.ID); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil, nil, ErrUserNotFound
		}
		return nil, nil, nil, err
	}

	user.UpdatedAt = requestAt
	user.LastActivatedAt = requestAt

	query = "UPDATE users SET updated_at=?, last_activated_at=? WHERE id=?"
	if _, err := tx.Exec(query, requestAt, requestAt, userID); err != nil {
		return nil, nil, nil, err
	}

	return user, loginBonuses, allPresents, nil
}

// isCompleteTodayLogin 当日分のログイン処理が終わっているかを確認する
func isCompleteTodayLogin(lastActivatedAt, requestAt time.Time) bool {
	return lastActivatedAt.Year() == requestAt.Year() &&
		lastActivatedAt.Month() == requestAt.Month() &&
		lastActivatedAt.Day() == requestAt.Day()
}

// obtainLoginBonus ログインボーナス付与
func (h *Handler) obtainLoginBonus(tx *sqlx.Tx, userID int64, requestAt int64) ([]*UserLoginBonus, error) {
	loginBonuses := make([]*LoginBonusMaster, 0)
	query := "SELECT * FROM login_bonus_masters WHERE start_at <= ? AND end_at >= ?"
	if err := tx.Select(&loginBonuses, query, requestAt, requestAt); err != nil {
		return nil, err
	}

	if len(loginBonuses) == 0 {
		return make([]*UserLoginBonus, 0), nil
	}

	// ログインボーナスIDを一括取得
	bonusIDs := make([]int64, len(loginBonuses))
	for i, bonus := range loginBonuses {
		bonusIDs[i] = bonus.ID
	}

	// 既存のユーザーログインボーナスを一括取得
	query = "SELECT * FROM user_login_bonuses WHERE user_id=? AND login_bonus_id IN (?)"
	query, params, err := sqlx.In(query, userID, bonusIDs)
	if err != nil {
		return nil, err
	}

	existingBonuses := make([]*UserLoginBonus, 0)
	if err := tx.Select(&existingBonuses, query, params...); err != nil {
		return nil, err
	}

	// 既存ボーナスをマップ化
	existingMap := make(map[int64]*UserLoginBonus)
	for _, bonus := range existingBonuses {
		existingMap[bonus.LoginBonusID] = bonus
	}

	sendLoginBonuses := make([]*UserLoginBonus, 0)
	rewardItems := make([]*LoginBonusRewardMaster, 0)

	// 各ログインボーナスを処理
	for _, bonus := range loginBonuses {
		userBonus, exists := existingMap[bonus.ID]
		initBonus := !exists

		if !exists {
			ubID, err := h.generateID()
			if err != nil {
				return nil, err
			}
			userBonus = &UserLoginBonus{
				ID:                 ubID,
				UserID:             userID,
				LoginBonusID:       bonus.ID,
				LastRewardSequence: 0,
				LoopCount:          1,
				CreatedAt:          requestAt,
				UpdatedAt:          requestAt,
			}
		}

		// ボーナス進捗更新
		if userBonus.LastRewardSequence < bonus.ColumnCount {
			userBonus.LastRewardSequence++
		} else {
			if bonus.Looped {
				userBonus.LoopCount += 1
				userBonus.LastRewardSequence = 1
			} else {
				// 上限まで付与完了しているボーナス
				continue
			}
		}
		userBonus.UpdatedAt = requestAt

		// 報酬アイテム情報を収集（後でバッチ取得）
		rewardItems = append(rewardItems, &LoginBonusRewardMaster{
			LoginBonusID:   bonus.ID,
			RewardSequence: userBonus.LastRewardSequence,
		})

		// 進捗の保存
		if initBonus {
			query = "INSERT INTO user_login_bonuses(id, user_id, login_bonus_id, last_reward_sequence, loop_count, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)"
			if _, err = tx.Exec(query, userBonus.ID, userBonus.UserID, userBonus.LoginBonusID, userBonus.LastRewardSequence, userBonus.LoopCount, userBonus.CreatedAt, userBonus.UpdatedAt); err != nil {
				return nil, err
			}
		} else {
			query = "UPDATE user_login_bonuses SET last_reward_sequence=?, loop_count=?, updated_at=? WHERE id=?"
			if _, err = tx.Exec(query, userBonus.LastRewardSequence, userBonus.LoopCount, userBonus.UpdatedAt, userBonus.ID); err != nil {
				return nil, err
			}
		}

		sendLoginBonuses = append(sendLoginBonuses, userBonus)
	}

	// 報酬アイテムを一括取得（キャッシュ活用）
	if len(rewardItems) > 0 {
		rewardMap := make(map[string]*LoginBonusRewardMaster)
		missingRewards := make([]*LoginBonusRewardMaster, 0)

		// まずキャッシュから取得を試行
		for _, reward := range rewardItems {
			if cachedReward, exists := h.Cache.GetLoginBonusReward(reward.LoginBonusID, reward.RewardSequence); exists {
				key := fmt.Sprintf("%d_%d", reward.LoginBonusID, reward.RewardSequence)
				rewardMap[key] = cachedReward
			} else {
				missingRewards = append(missingRewards, reward)
			}
		}

		// キャッシュにないものはDBから取得
		if len(missingRewards) > 0 {
			rewardConditions := make([]string, len(missingRewards))
			rewardParams := make([]interface{}, 0, len(missingRewards)*2)

			for i, reward := range missingRewards {
				rewardConditions[i] = "(login_bonus_id=? AND reward_sequence=?)"
				rewardParams = append(rewardParams, reward.LoginBonusID, reward.RewardSequence)
			}

			query = fmt.Sprintf("SELECT * FROM login_bonus_reward_masters WHERE %s",
				strings.Join(rewardConditions, " OR "))

			actualRewards := make([]*LoginBonusRewardMaster, 0)
			if err := tx.Select(&actualRewards, query, rewardParams...); err != nil {
				return nil, err
			}

			// DBから取得したものをキャッシュに保存し、マップに追加
			for _, reward := range actualRewards {
				h.Cache.SetLoginBonusReward(reward)
				key := fmt.Sprintf("%d_%d", reward.LoginBonusID, reward.RewardSequence)
				rewardMap[key] = reward
			}
		}

		// アイテム付与をバッチ処理用に準備
		presents := make([]*UserPresent, 0)
		for _, userBonus := range sendLoginBonuses {
			key := fmt.Sprintf("%d_%d", userBonus.LoginBonusID, userBonus.LastRewardSequence)
			rewardItem, exists := rewardMap[key]
			if !exists {
				return nil, ErrLoginBonusRewardNotFound
			}

			// プレゼント形式でアイテム付与情報を作成
			presents = append(presents, &UserPresent{
				ItemType: rewardItem.ItemType,
				ItemID:   rewardItem.ItemID,
				Amount:   int(rewardItem.Amount),
			})
		}

		// バッチでアイテム付与
		if len(presents) > 0 {
			err = h.obtainItemsBatch(tx, presents, userID, requestAt)
			if err != nil {
				return nil, err
			}
		}
	}

	return sendLoginBonuses, nil
}

// obtainPresent プレゼント付与
func (h *Handler) obtainPresent(tx *sqlx.Tx, userID int64, requestAt int64) ([]*UserPresent, error) {
	normalPresents := make([]*PresentAllMaster, 0)
	query := "SELECT * FROM present_all_masters WHERE registered_start_at <= ? AND registered_end_at >= ?"
	if err := tx.Select(&normalPresents, query, requestAt, requestAt); err != nil {
		return nil, err
	}

	if len(normalPresents) == 0 {
		return make([]*UserPresent, 0), nil
	}

	// プレゼントIDを一括取得
	presentIDs := make([]int64, len(normalPresents))
	for i, np := range normalPresents {
		presentIDs[i] = np.ID
	}

	// 既に受け取ったプレゼント履歴を一括取得
	query = "SELECT present_all_id FROM user_present_all_received_history WHERE user_id=? AND present_all_id IN (?)"
	query, params, err := sqlx.In(query, userID, presentIDs)
	if err != nil {
		return nil, err
	}

	receivedIDs := make([]int64, 0)
	if err := tx.Select(&receivedIDs, query, params...); err != nil {
		return nil, err
	}

	// 受け取り済みIDをマップ化
	receivedMap := make(map[int64]bool)
	for _, id := range receivedIDs {
		receivedMap[id] = true
	}

	// 未受け取りのプレゼントを処理
	obtainPresents := make([]*UserPresent, 0)
	histories := make([]*UserPresentAllReceivedHistory, 0)

	for _, np := range normalPresents {
		if receivedMap[np.ID] {
			// プレゼント配布済
			continue
		}

		pID, err := h.generateID()
		if err != nil {
			return nil, err
		}
		up := &UserPresent{
			ID:             pID,
			UserID:         userID,
			SentAt:         requestAt,
			ItemType:       np.ItemType,
			ItemID:         np.ItemID,
			Amount:         int(np.Amount),
			PresentMessage: np.PresentMessage,
			CreatedAt:      requestAt,
			UpdatedAt:      requestAt,
		}

		phID, err := h.generateID()
		if err != nil {
			return nil, err
		}
		history := &UserPresentAllReceivedHistory{
			ID:           phID,
			UserID:       userID,
			PresentAllID: np.ID,
			ReceivedAt:   requestAt,
			CreatedAt:    requestAt,
			UpdatedAt:    requestAt,
		}

		obtainPresents = append(obtainPresents, up)
		histories = append(histories, history)
	}

	// プレゼントを一括挿入
	if len(obtainPresents) > 0 {
		for _, up := range obtainPresents {
			query = "INSERT INTO user_presents(id, user_id, sent_at, item_type, item_id, amount, present_message, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)"
			if _, err := tx.Exec(query, up.ID, up.UserID, up.SentAt, up.ItemType, up.ItemID, up.Amount, up.PresentMessage, up.CreatedAt, up.UpdatedAt); err != nil {
				return nil, err
			}
		}

		// 履歴を一括挿入
		for _, history := range histories {
			query = "INSERT INTO user_present_all_received_history(id, user_id, present_all_id, received_at, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)"
			if _, err := tx.Exec(query, history.ID, history.UserID, history.PresentAllID, history.ReceivedAt, history.CreatedAt, history.UpdatedAt); err != nil {
				return nil, err
			}
		}
	}

	return obtainPresents, nil
}

// obtainItem アイテム付与処理
func (h *Handler) obtainItem(tx *sqlx.Tx, userID, itemID int64, itemType int, obtainAmount int64, requestAt int64) ([]int64, []*UserCard, []*UserItem, error) {
	obtainCoins := make([]int64, 0)
	obtainCards := make([]*UserCard, 0)
	obtainItems := make([]*UserItem, 0)

	switch itemType {
	case 1: // coin
		user := new(User)
		query := "SELECT * FROM users WHERE id=?"
		if err := tx.Get(user, query, userID); err != nil {
			if err == sql.ErrNoRows {
				return nil, nil, nil, ErrUserNotFound
			}
			return nil, nil, nil, err
		}

		query = "UPDATE users SET isu_coin=? WHERE id=?"
		totalCoin := user.IsuCoin + obtainAmount
		if _, err := tx.Exec(query, totalCoin, user.ID); err != nil {
			return nil, nil, nil, err
		}
		obtainCoins = append(obtainCoins, obtainAmount)

	case 2: // card(ハンマー)
		query := "SELECT * FROM item_masters WHERE id=? AND item_type=?"
		item := new(ItemMaster)
		if err := tx.Get(item, query, itemID, itemType); err != nil {
			if err == sql.ErrNoRows {
				return nil, nil, nil, ErrItemNotFound
			}
			return nil, nil, nil, err
		}

		cID, err := h.generateID()
		if err != nil {
			return nil, nil, nil, err
		}
		card := &UserCard{
			ID:           cID,
			UserID:       userID,
			CardID:       item.ID,
			AmountPerSec: *item.AmountPerSec,
			Level:        1,
			TotalExp:     0,
			CreatedAt:    requestAt,
			UpdatedAt:    requestAt,
		}
		query = "INSERT INTO user_cards(id, user_id, card_id, amount_per_sec, level, total_exp, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)"
		if _, err := tx.Exec(query, card.ID, card.UserID, card.CardID, card.AmountPerSec, card.Level, card.TotalExp, card.CreatedAt, card.UpdatedAt); err != nil {
			return nil, nil, nil, err
		}
		obtainCards = append(obtainCards, card)

	case 3, 4: // 強化素材
		query := "SELECT * FROM item_masters WHERE id=? AND item_type=?"
		item := new(ItemMaster)
		if err := tx.Get(item, query, itemID, itemType); err != nil {
			if err == sql.ErrNoRows {
				return nil, nil, nil, ErrItemNotFound
			}
			return nil, nil, nil, err
		}

		query = "SELECT * FROM user_items WHERE user_id=? AND item_id=?"
		uitem := new(UserItem)
		if err := tx.Get(uitem, query, userID, item.ID); err != nil {
			if err != sql.ErrNoRows {
				return nil, nil, nil, err
			}
			uitem = nil
		}

		if uitem == nil {
			uitemID, err := h.generateID()
			if err != nil {
				return nil, nil, nil, err
			}
			uitem = &UserItem{
				ID:        uitemID,
				UserID:    userID,
				ItemType:  item.ItemType,
				ItemID:    item.ID,
				Amount:    int(obtainAmount),
				CreatedAt: requestAt,
				UpdatedAt: requestAt,
			}
			query = "INSERT INTO user_items(id, user_id, item_id, item_type, amount, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)"
			if _, err := tx.Exec(query, uitem.ID, userID, uitem.ItemID, uitem.ItemType, uitem.Amount, requestAt, requestAt); err != nil {
				return nil, nil, nil, err
			}

		} else {
			uitem.Amount += int(obtainAmount)
			uitem.UpdatedAt = requestAt
			query = "UPDATE user_items SET amount=?, updated_at=? WHERE id=?"
			if _, err := tx.Exec(query, uitem.Amount, uitem.UpdatedAt, uitem.ID); err != nil {
				return nil, nil, nil, err
			}
		}

		obtainItems = append(obtainItems, uitem)

	default:
		return nil, nil, nil, ErrInvalidItemType
	}

	return obtainCoins, obtainCards, obtainItems, nil
}

// obtainItemsBatch アイテム付与処理のバッチ版
func (h *Handler) obtainItemsBatch(tx *sqlx.Tx, presents []*UserPresent, userID int64, requestAt int64) error {
	// アイテム種別ごとにグループ化
	coinTotal := int64(0)
	cardItems := make([]*UserPresent, 0)
	materialItems := make(map[int64]int64) // item_id -> total_amount

	for _, present := range presents {
		switch present.ItemType {
		case 1: // coin
			coinTotal += int64(present.Amount)
		case 2: // card(ハンマー)
			cardItems = append(cardItems, present)
		case 3, 4: // 強化素材
			materialItems[present.ItemID] += int64(present.Amount)
		}
	}

	// コインの一括更新
	if coinTotal > 0 {
		query := "UPDATE users SET isu_coin = isu_coin + ? WHERE id = ?"
		if _, err := tx.Exec(query, coinTotal, userID); err != nil {
			return err
		}
	}

	// カードの一括挿入
	if len(cardItems) > 0 {
		// カードマスター情報を一括取得（キャッシュ活用）
		masterMap := make(map[int64]*ItemMaster)
		missingCardIDs := make([]int64, 0)

		// まずキャッシュから取得を試行
		for _, item := range cardItems {
			if cachedMaster, exists := h.Cache.GetItemMaster(item.ItemID); exists {
				masterMap[item.ItemID] = cachedMaster
			} else {
				missingCardIDs = append(missingCardIDs, item.ItemID)
			}
		}

		// キャッシュにないものはDBから取得
		if len(missingCardIDs) > 0 {
			query := "SELECT * FROM item_masters WHERE id IN (?) AND item_type = 2"
			query, params, err := sqlx.In(query, missingCardIDs)
			if err != nil {
				return err
			}

			itemMasters := make([]*ItemMaster, 0)
			if err := tx.Select(&itemMasters, query, params...); err != nil {
				return err
			}

			// DBから取得したものをキャッシュに保存し、マップに追加
			for _, master := range itemMasters {
				h.Cache.SetItemMaster(master)
				masterMap[master.ID] = master
			}
		}

		// カードを一括挿入（真のバルク処理）
		cardInserts := make([]*UserCard, 0)
		for _, item := range cardItems {
			master, exists := masterMap[item.ItemID]
			if !exists {
				return ErrItemNotFound
			}

			for i := 0; i < item.Amount; i++ {
				cID, err := h.generateID()
				if err != nil {
					return err
				}

				cardInserts = append(cardInserts, &UserCard{
					ID:           cID,
					UserID:       userID,
					CardID:       master.ID,
					AmountPerSec: *master.AmountPerSec,
					Level:        1,
					TotalExp:     0,
					CreatedAt:    requestAt,
					UpdatedAt:    requestAt,
				})
			}
		}

		// NamedExecを使った一括INSERT
		if len(cardInserts) > 0 {
			query := `INSERT INTO user_cards(id, user_id, card_id, amount_per_sec, level, total_exp, created_at, updated_at)
					  VALUES (:id, :user_id, :card_id, :amount_per_sec, :level, :total_exp, :created_at, :updated_at)`

			if _, err := tx.NamedExec(query, cardInserts); err != nil {
				return err
			}
		}
	}

	// 強化素材の一括更新
	if len(materialItems) > 0 {
		// 既存のアイテムを取得
		itemIDs := make([]int64, 0, len(materialItems))
		for itemID := range materialItems {
			itemIDs = append(itemIDs, itemID)
		}

		query := "SELECT * FROM user_items WHERE user_id = ? AND item_id IN (?)"
		query, params, err := sqlx.In(query, userID, itemIDs)
		if err != nil {
			return err
		}

		existingItems := make([]*UserItem, 0)
		if err := tx.Select(&existingItems, query, params...); err != nil {
			return err
		}

		// 既存アイテムをマップ化
		existingMap := make(map[int64]*UserItem)
		for _, item := range existingItems {
			existingMap[item.ItemID] = item
		}

		// アイテムマスター情報を取得
		query = "SELECT * FROM item_masters WHERE id IN (?) AND item_type IN (3, 4)"
		query, params, err = sqlx.In(query, itemIDs)
		if err != nil {
			return err
		}

		itemMasters := make([]*ItemMaster, 0)
		if err := tx.Select(&itemMasters, query, params...); err != nil {
			return err
		}

		masterMap := make(map[int64]*ItemMaster)
		for _, master := range itemMasters {
			masterMap[master.ID] = master
		}

		// 更新・挿入処理（NamedExec使用）
		updateItems := make([]*UserItem, 0)
		insertItems := make([]*UserItem, 0)

		for itemID, amount := range materialItems {
			master, exists := masterMap[itemID]
			if !exists {
				return ErrItemNotFound
			}

			if existingItem, exists := existingMap[itemID]; exists {
				// 既存アイテムの更新
				existingItem.Amount += int(amount)
				existingItem.UpdatedAt = requestAt
				updateItems = append(updateItems, existingItem)
			} else {
				// 新規アイテムの挿入
				uitemID, err := h.generateID()
				if err != nil {
					return err
				}

				insertItems = append(insertItems, &UserItem{
					ID:        uitemID,
					UserID:    userID,
					ItemID:    itemID,
					ItemType:  master.ItemType,
					Amount:    int(amount),
					CreatedAt: requestAt,
					UpdatedAt: requestAt,
				})
			}
		}

		// 一括UPDATE（CASE文使用、sqlx.Inで安全に構築）
		if len(updateItems) > 0 {
			// IDリストを作成
			ids := make([]int64, len(updateItems))
			caseWhenAmount := make([]string, len(updateItems))
			caseWhenUpdated := make([]string, len(updateItems))

			for i, item := range updateItems {
				ids[i] = item.ID
				caseWhenAmount[i] = fmt.Sprintf("WHEN %d THEN %d", item.ID, item.Amount)
				caseWhenUpdated[i] = fmt.Sprintf("WHEN %d THEN %d", item.ID, item.UpdatedAt)
			}

			// sqlx.Inを使って安全にIN句を構築
			baseQuery := fmt.Sprintf(`UPDATE user_items SET
				amount = CASE id %s END,
				updated_at = CASE id %s END
				WHERE id IN (?)`,
				strings.Join(caseWhenAmount, " "),
				strings.Join(caseWhenUpdated, " "))

			// updateArgsとidsを結合（型変換が必要）
			idsInterface := make([]interface{}, len(ids))
			for i, id := range ids {
				idsInterface[i] = id
			}
			query, params, err := sqlx.In(baseQuery, idsInterface)
			if err != nil {
				return err
			}

			if _, err := tx.Exec(query, params...); err != nil {
				return err
			}
		}

		// NamedExecを使った一括INSERT
		if len(insertItems) > 0 {
			query := `INSERT INTO user_items(id, user_id, item_id, item_type, amount, created_at, updated_at)
					  VALUES (:id, :user_id, :item_id, :item_type, :amount, :created_at, :updated_at)`

			if _, err := tx.NamedExec(query, insertItems); err != nil {
				return err
			}
		}
	}

	return nil
}

// initialize 初期化処理
// POST /initialize
func initialize(c echo.Context) error {
	dbx, err := connectDB(true)
	if err != nil {
		return errorResponse(c, http.StatusInternalServerError, err)
	}
	defer dbx.Close()

	errCh := make(chan error, len(dbHosts))
	wg := sync.WaitGroup{}

	defer close(errCh)

	for _, host := range dbHosts {
		wg.Add(1)
		go func(host string) {
			defer wg.Done()

			resp, err := http.Post(fmt.Sprintf("http://%s:8080/initializeOne", host), "application/json", nil)
			if err != nil {
				errCh <- err
				return
			}

			if resp.StatusCode != http.StatusOK {
				errCh <- fmt.Errorf("CODE: %d", resp.StatusCode)
				return
			}
		}(host)
	}

	wg.Wait()
	if len(errCh) > 0 {
		return errorResponse(c, http.StatusInternalServerError, <-errCh)
	}

	return successResponse(c, &InitializeResponse{
		Language: "go",
	})
}

func initializeOne(c echo.Context) error {
	out, err := exec.Command("/bin/sh", "-c", SQLDirectory+"init.sh").CombinedOutput()
	if err != nil {
		c.Logger().Errorf("init.sh 実行失敗: %s\nエラー: %v", string(out), err)
		return errorResponse(c, http.StatusInternalServerError, err)
	}

	c.Logger().Infof("init.sh 実行成功: %s", string(out))
	// キャッシュをクリア（グローバルなキャッシュインスタンスがある場合）
	// 注意: この実装では各Handlerインスタンスが独自のキャッシュを持つため、
	// 実際の運用では全てのHandlerインスタンスのキャッシュをクリアする必要がある

	return successResponse(c, &InitializeResponse{
		Language: "go",
	})
}

type InitializeResponse struct {
	Language string `json:"language"`
}

// createUser ユーザの作成
// POST /user
func (h *Handler) createUser(c echo.Context) error {
	defer c.Request().Body.Close()
	req := new(CreateUserRequest)
	if err := parseRequestBody(c, req); err != nil {
		return errorResponse(c, http.StatusBadRequest, err)
	}

	if req.ViewerID == "" || req.PlatformType < 1 || req.PlatformType > 3 {
		return errorResponse(c, http.StatusBadRequest, ErrInvalidRequestBody)
	}

	requestAt, err := getRequestTime(c)
	if err != nil {
		return errorResponse(c, http.StatusInternalServerError, ErrGetRequestTime)
	}

	// ユーザ作成
	uID, err := h.generateID()
	if err != nil {
		return errorResponse(c, http.StatusInternalServerError, err)
	}

	// ユーザーIDに基づいて適切なDBを選択
	db := h.getDBForUserID(uID)

	tx, err := db.Beginx()
	if err != nil {
		return errorResponse(c, http.StatusInternalServerError, err)
	}
	defer tx.Rollback() //nolint:errcheck
	user := &User{
		ID:              uID,
		IsuCoin:         0,
		LastGetRewardAt: requestAt,
		LastActivatedAt: requestAt,
		RegisteredAt:    requestAt,
		CreatedAt:       requestAt,
		UpdatedAt:       requestAt,
	}
	query := "INSERT INTO users(id, last_activated_at, registered_at, last_getreward_at, created_at, updated_at) VALUES(?, ?, ?, ?, ?, ?)"
	if _, err = tx.Exec(query, user.ID, user.LastActivatedAt, user.RegisteredAt, user.LastGetRewardAt, user.CreatedAt, user.UpdatedAt); err != nil {
		return errorResponse(c, http.StatusInternalServerError, err)
	}

	udID, err := h.generateID()
	if err != nil {
		return errorResponse(c, http.StatusInternalServerError, err)
	}
	userDevice := &UserDevice{
		ID:           udID,
		UserID:       user.ID,
		PlatformID:   req.ViewerID,
		PlatformType: req.PlatformType,
		CreatedAt:    requestAt,
		UpdatedAt:    requestAt,
	}
	query = "INSERT INTO user_devices(id, user_id, platform_id, platform_type, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)"
	_, err = tx.Exec(query, userDevice.ID, user.ID, req.ViewerID, req.PlatformType, requestAt, requestAt)
	if err != nil {
		return errorResponse(c, http.StatusInternalServerError, err)
	}

	// 初期デッキ付与
	initCard := new(ItemMaster)
	query = "SELECT * FROM item_masters WHERE id=?"
	if err = tx.Get(initCard, query, 2); err != nil {
		if err == sql.ErrNoRows {
			return errorResponse(c, http.StatusNotFound, ErrItemNotFound)
		}
		return errorResponse(c, http.StatusInternalServerError, err)
	}

	initCards := make([]*UserCard, 0, 3)
	for i := 0; i < 3; i++ {
		cID, err := h.generateID()
		if err != nil {
			return errorResponse(c, http.StatusInternalServerError, err)
		}
		card := &UserCard{
			ID:           cID,
			UserID:       user.ID,
			CardID:       initCard.ID,
			AmountPerSec: *initCard.AmountPerSec,
			Level:        1,
			TotalExp:     0,
			CreatedAt:    requestAt,
			UpdatedAt:    requestAt,
		}
		query = "INSERT INTO user_cards(id, user_id, card_id, amount_per_sec, level, total_exp, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)"
		if _, err := tx.Exec(query, card.ID, card.UserID, card.CardID, card.AmountPerSec, card.Level, card.TotalExp, card.CreatedAt, card.UpdatedAt); err != nil {
			return errorResponse(c, http.StatusInternalServerError, err)
		}
		initCards = append(initCards, card)
	}

	deckID, err := h.generateID()
	if err != nil {
		return errorResponse(c, http.StatusInternalServerError, err)
	}
	initDeck := &UserDeck{
		ID:        deckID,
		UserID:    user.ID,
		CardID1:   initCards[0].ID,
		CardID2:   initCards[1].ID,
		CardID3:   initCards[2].ID,
		CreatedAt: requestAt,
		UpdatedAt: requestAt,
	}
	query = "INSERT INTO user_decks(id, user_id, user_card_id_1, user_card_id_2, user_card_id_3, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)"
	if _, err := tx.Exec(query, initDeck.ID, initDeck.UserID, initDeck.CardID1, initDeck.CardID2, initDeck.CardID3, initDeck.CreatedAt, initDeck.UpdatedAt); err != nil {
		return errorResponse(c, http.StatusInternalServerError, err)
	}

	// ログイン処理
	user, loginBonuses, presents, err := h.loginProcess(tx, user.ID, requestAt)
	if err != nil {
		if err == ErrUserNotFound || err == ErrItemNotFound || err == ErrLoginBonusRewardNotFound {
			return errorResponse(c, http.StatusNotFound, err)
		}
		if err == ErrInvalidItemType {
			return errorResponse(c, http.StatusBadRequest, err)
		}
		return errorResponse(c, http.StatusInternalServerError, err)
	}

	// セッション発行
	sID, err := h.generateID()
	if err != nil {
		return errorResponse(c, http.StatusInternalServerError, err)
	}
	sessID, err := generateUUID()
	if err != nil {
		return errorResponse(c, http.StatusInternalServerError, err)
	}
	sess := &Session{
		ID:        sID,
		UserID:    user.ID,
		SessionID: sessID,
		CreatedAt: requestAt,
		UpdatedAt: requestAt,
		ExpiredAt: requestAt + 86400,
	}
	query = "INSERT INTO user_sessions(id, user_id, session_id, created_at, updated_at, expired_at) VALUES (?, ?, ?, ?, ?, ?)"
	if _, err = tx.Exec(query, sess.ID, sess.UserID, sess.SessionID, sess.CreatedAt, sess.UpdatedAt, sess.ExpiredAt); err != nil {
		return errorResponse(c, http.StatusInternalServerError, err)
	}

	err = tx.Commit()
	if err != nil {
		return errorResponse(c, http.StatusInternalServerError, err)
	}

	return successResponse(c, &CreateUserResponse{
		UserID:           user.ID,
		ViewerID:         req.ViewerID,
		SessionID:        sess.SessionID,
		CreatedAt:        requestAt,
		UpdatedResources: makeUpdatedResources(requestAt, user, userDevice, initCards, []*UserDeck{initDeck}, nil, loginBonuses, presents),
	})
}

type CreateUserRequest struct {
	ViewerID     string `json:"viewerId"`
	PlatformType int    `json:"platformType"`
}

type CreateUserResponse struct {
	UserID           int64            `json:"userId"`
	ViewerID         string           `json:"viewerId"`
	SessionID        string           `json:"sessionId"`
	CreatedAt        int64            `json:"createdAt"`
	UpdatedResources *UpdatedResource `json:"updatedResources"`
}

// login ログイン
// POST /login
func (h *Handler) login(c echo.Context) error {
	defer c.Request().Body.Close()
	req := new(LoginRequest)
	if err := parseRequestBody(c, req); err != nil {
		return errorResponse(c, http.StatusBadRequest, err)
	}

	requestAt, err := getRequestTime(c)
	if err != nil {
		return errorResponse(c, http.StatusInternalServerError, ErrGetRequestTime)
	}

	// ユーザーIDに基づいて適切なDBを選択
	db := h.getDBForUserID(req.UserID)

	user := new(User)
	query := "SELECT * FROM users WHERE id=?"
	if err := db.Get(user, query, req.UserID); err != nil {
		if err == sql.ErrNoRows {
			return errorResponse(c, http.StatusNotFound, ErrUserNotFound)
		}
		return errorResponse(c, http.StatusInternalServerError, err)
	}

	isBan, err := h.checkBan(user.ID)
	if err != nil {
		return errorResponse(c, http.StatusInternalServerError, err)
	}
	if isBan {
		return errorResponse(c, http.StatusForbidden, ErrForbidden)
	}

	if err = h.checkViewerID(user.ID, req.ViewerID); err != nil {
		if err == ErrUserDeviceNotFound {
			return errorResponse(c, http.StatusNotFound, err)
		}
		return errorResponse(c, http.StatusInternalServerError, err)
	}

	tx, err := db.Beginx()
	if err != nil {
		return errorResponse(c, http.StatusInternalServerError, err)
	}
	defer tx.Rollback() //nolint:errcheck

	query = "UPDATE user_sessions SET deleted_at=? WHERE user_id=? AND deleted_at IS NULL"
	if _, err = tx.Exec(query, requestAt, req.UserID); err != nil {
		return errorResponse(c, http.StatusInternalServerError, err)
	}
	sID, err := h.generateID()
	if err != nil {
		return errorResponse(c, http.StatusInternalServerError, err)
	}
	sessID, err := generateUUID()
	if err != nil {
		return errorResponse(c, http.StatusInternalServerError, err)
	}
	sess := &Session{
		ID:        sID,
		UserID:    req.UserID,
		SessionID: sessID,
		CreatedAt: requestAt,
		UpdatedAt: requestAt,
		ExpiredAt: requestAt + 86400,
	}
	query = "INSERT INTO user_sessions(id, user_id, session_id, created_at, updated_at, expired_at) VALUES (?, ?, ?, ?, ?, ?)"
	if _, err = tx.Exec(query, sess.ID, sess.UserID, sess.SessionID, sess.CreatedAt, sess.UpdatedAt, sess.ExpiredAt); err != nil {
		return errorResponse(c, http.StatusInternalServerError, err)
	}

	// 同日にすでにログインしているユーザはログイン処理をしない
	if isCompleteTodayLogin(time.Unix(user.LastActivatedAt, 0), time.Unix(requestAt, 0)) {
		user.UpdatedAt = requestAt
		user.LastActivatedAt = requestAt

		query = "UPDATE users SET updated_at=?, last_activated_at=? WHERE id=?"
		if _, err := tx.Exec(query, requestAt, requestAt, req.UserID); err != nil {
			return errorResponse(c, http.StatusInternalServerError, err)
		}

		err = tx.Commit()
		if err != nil {
			return errorResponse(c, http.StatusInternalServerError, err)
		}

		return successResponse(c, &LoginResponse{
			ViewerID:         req.ViewerID,
			SessionID:        sess.SessionID,
			UpdatedResources: makeUpdatedResources(requestAt, user, nil, nil, nil, nil, nil, nil),
		})
	}

	user, loginBonuses, presents, err := h.loginProcess(tx, req.UserID, requestAt)
	if err != nil {
		if err == ErrUserNotFound || err == ErrItemNotFound || err == ErrLoginBonusRewardNotFound {
			return errorResponse(c, http.StatusNotFound, err)
		}
		if err == ErrInvalidItemType {
			return errorResponse(c, http.StatusBadRequest, err)
		}
		return errorResponse(c, http.StatusInternalServerError, err)
	}

	err = tx.Commit()
	if err != nil {
		return errorResponse(c, http.StatusInternalServerError, err)
	}

	return successResponse(c, &LoginResponse{
		ViewerID:         req.ViewerID,
		SessionID:        sess.SessionID,
		UpdatedResources: makeUpdatedResources(requestAt, user, nil, nil, nil, nil, loginBonuses, presents),
	})
}

type LoginRequest struct {
	ViewerID string `json:"viewerId"`
	UserID   int64  `json:"userId"`
}

type LoginResponse struct {
	ViewerID         string           `json:"viewerId"`
	SessionID        string           `json:"sessionId"`
	UpdatedResources *UpdatedResource `json:"updatedResources"`
}

// listGacha ガチャ一覧
// GET /user/{userID}/gacha/index
func (h *Handler) listGacha(c echo.Context) error {
	userID, err := getUserID(c)
	if err != nil {
		return errorResponse(c, http.StatusBadRequest, err)
	}

	requestAt, err := getRequestTime(c)
	if err != nil {
		return errorResponse(c, http.StatusInternalServerError, ErrGetRequestTime)
	}

	gachaMasterList := []*GachaMaster{}
	query := "SELECT * FROM gacha_masters WHERE start_at <= ? AND end_at >= ? ORDER BY display_order ASC"
	err = h.DB.Select(&gachaMasterList, query, requestAt, requestAt)
	if err != nil {
		return errorResponse(c, http.StatusInternalServerError, err)
	}

	if len(gachaMasterList) == 0 {
		return successResponse(c, &ListGachaResponse{
			Gachas: []*GachaData{},
		})
	}

	gachaDataList := make([]*GachaData, 0)
	query = "SELECT * FROM gacha_item_masters WHERE gacha_id=? ORDER BY id ASC"
	for _, v := range gachaMasterList {
		var gachaItem []*GachaItemMaster
		err = h.DB.Select(&gachaItem, query, v.ID)
		if err != nil {
			return errorResponse(c, http.StatusInternalServerError, err)
		}

		if len(gachaItem) == 0 {
			return errorResponse(c, http.StatusNotFound, fmt.Errorf("not found gacha item"))
		}

		gachaDataList = append(gachaDataList, &GachaData{
			Gacha:     v,
			GachaItem: gachaItem,
		})
	}

	// ガチャ実行用のワンタイムトークンの発行
	query = "UPDATE user_one_time_tokens SET deleted_at=? WHERE user_id=? AND deleted_at IS NULL"
	if _, err = h.DB.Exec(query, requestAt, userID); err != nil {
		return errorResponse(c, http.StatusInternalServerError, err)
	}
	tID, err := h.generateID()
	if err != nil {
		return errorResponse(c, http.StatusInternalServerError, err)
	}
	tk, err := generateUUID()
	if err != nil {
		return errorResponse(c, http.StatusInternalServerError, err)
	}
	token := &UserOneTimeToken{
		ID:        tID,
		UserID:    userID,
		Token:     tk,
		TokenType: 1,
		CreatedAt: requestAt,
		UpdatedAt: requestAt,
		ExpiredAt: requestAt + 600,
	}
	query = "INSERT INTO user_one_time_tokens(id, user_id, token, token_type, created_at, updated_at, expired_at) VALUES (?, ?, ?, ?, ?, ?, ?)"
	if _, err = h.DB.Exec(query, token.ID, token.UserID, token.Token, token.TokenType, token.CreatedAt, token.UpdatedAt, token.ExpiredAt); err != nil {
		return errorResponse(c, http.StatusInternalServerError, err)
	}

	// キャッシュにも保存
	h.TokenCache.SetToken(token.Token, token.UserID, token.TokenType, token.ExpiredAt, token.CreatedAt)

	return successResponse(c, &ListGachaResponse{
		OneTimeToken: token.Token,
		Gachas:       gachaDataList,
	})
}

type ListGachaResponse struct {
	OneTimeToken string       `json:"oneTimeToken"`
	Gachas       []*GachaData `json:"gachas"`
}

type GachaData struct {
	Gacha     *GachaMaster       `json:"gacha"`
	GachaItem []*GachaItemMaster `json:"gachaItemList"`
}

// drawGacha ガチャを引く
// POST /user/{userID}/gacha/draw/{gachaID}/{n}
func (h *Handler) drawGacha(c echo.Context) error {
	userID, err := getUserID(c)
	if err != nil {
		return errorResponse(c, http.StatusBadRequest, err)
	}

	gachaID := c.Param("gachaID")
	if gachaID == "" {
		return errorResponse(c, http.StatusBadRequest, fmt.Errorf("invalid gachaID"))
	}

	gachaCount, err := strconv.ParseInt(c.Param("n"), 10, 64)
	if err != nil {
		return errorResponse(c, http.StatusBadRequest, err)
	}
	if gachaCount != 1 && gachaCount != 10 {
		return errorResponse(c, http.StatusBadRequest, fmt.Errorf("invalid draw gacha times"))
	}

	defer c.Request().Body.Close()
	req := new(DrawGachaRequest)
	if err = parseRequestBody(c, req); err != nil {
		return errorResponse(c, http.StatusBadRequest, err)
	}

	requestAt, err := getRequestTime(c)
	if err != nil {
		return errorResponse(c, http.StatusInternalServerError, ErrGetRequestTime)
	}

	if err = h.checkOneTimeToken(userID, req.OneTimeToken, 1, requestAt); err != nil {
		if err == ErrInvalidToken {
			return errorResponse(c, http.StatusBadRequest, err)
		}
		return errorResponse(c, http.StatusInternalServerError, err)
	}

	if err = h.checkViewerID(userID, req.ViewerID); err != nil {
		if err == ErrUserDeviceNotFound {
			return errorResponse(c, http.StatusNotFound, err)
		}
		return errorResponse(c, http.StatusInternalServerError, err)
	}

	consumedCoin := int64(gachaCount * 1000)

	user := new(User)
	query := "SELECT * FROM users WHERE id=?"
	if err := h.getDBForUserID(userID).Get(user, query, userID); err != nil {
		if err == sql.ErrNoRows {
			return errorResponse(c, http.StatusNotFound, ErrUserNotFound)
		}
		return errorResponse(c, http.StatusInternalServerError, err)
	}
	if user.IsuCoin < consumedCoin {
		return errorResponse(c, http.StatusConflict, fmt.Errorf("not enough isucon"))
	}

	query = "SELECT * FROM gacha_masters WHERE id=? AND start_at <= ? AND end_at >= ?"
	gachaInfo := new(GachaMaster)
	if err = h.DB.Get(gachaInfo, query, gachaID, requestAt, requestAt); err != nil {
		if sql.ErrNoRows == err {
			return errorResponse(c, http.StatusNotFound, fmt.Errorf("not found gacha"))
		}
		return errorResponse(c, http.StatusInternalServerError, err)
	}

	// gachaIDをint64に変換
	gachaIDInt, err := strconv.ParseInt(gachaID, 10, 64)
	if err != nil {
		return errorResponse(c, http.StatusBadRequest, fmt.Errorf("invalid gachaID"))
	}

	// キャッシュからガチャアイテムを取得
	gachaItemList, sum, cached := h.Cache.GetGachaItems(gachaIDInt)
	if !cached {
		// キャッシュにない場合はDBから取得
		gachaItemList = make([]*GachaItemMaster, 0)
		err = h.DB.Select(&gachaItemList, "SELECT * FROM gacha_item_masters WHERE gacha_id=? ORDER BY id ASC", gachaID)
		if err != nil {
			return errorResponse(c, http.StatusInternalServerError, err)
		}
		if len(gachaItemList) == 0 {
			return errorResponse(c, http.StatusNotFound, fmt.Errorf("not found gacha item"))
		}

		// キャッシュに保存
		h.Cache.SetGachaItems(gachaIDInt, gachaItemList)

		// weight合計値を再計算
		sum = 0
		for _, item := range gachaItemList {
			sum += int64(item.Weight)
		}
	}

	if sum == 0 {
		return errorResponse(c, http.StatusInternalServerError, fmt.Errorf("invalid gacha weight sum"))
	}

	// random値の導出 & 抽選
	result := make([]*GachaItemMaster, 0, gachaCount)
	for i := 0; i < int(gachaCount); i++ {
		random := rand.Int63n(sum)
		boundary := 0
		for _, v := range gachaItemList {
			boundary += v.Weight
			if random < int64(boundary) {
				result = append(result, v)
				break
			}
		}
	}

	// ユーザーIDに基づいて適切なDBを選択
	db := h.getDBForUserID(userID)

	tx, err := db.Beginx()
	if err != nil {
		return errorResponse(c, http.StatusInternalServerError, err)
	}
	defer tx.Rollback() //nolint:errcheck

	// プレゼントにガチャ結果を付与する（バッチ化）
	presents := make([]*UserPresent, 0, gachaCount)
	presentMessage := fmt.Sprintf("%sの付与アイテムです", gachaInfo.Name)

	for _, v := range result {
		pID, err := h.generateID()
		if err != nil {
			return errorResponse(c, http.StatusInternalServerError, err)
		}
		present := &UserPresent{
			ID:             pID,
			UserID:         userID,
			SentAt:         requestAt,
			ItemType:       v.ItemType,
			ItemID:         v.ItemID,
			Amount:         v.Amount,
			PresentMessage: presentMessage,
			CreatedAt:      requestAt,
			UpdatedAt:      requestAt,
		}
		presents = append(presents, present)
	}

	// プレゼントを一括挿入（NamedExecを使用）
	if len(presents) > 0 {
		query = `INSERT INTO user_presents(id, user_id, sent_at, item_type, item_id, amount, present_message, created_at, updated_at)
				 VALUES (:id, :user_id, :sent_at, :item_type, :item_id, :amount, :present_message, :created_at, :updated_at)`

		for _, present := range presents {
			if _, err := tx.NamedExec(query, present); err != nil {
				return errorResponse(c, http.StatusInternalServerError, err)
			}
		}
	}

	// コイン消費
	query = "UPDATE users SET isu_coin=? WHERE id=?"
	totalCoin := user.IsuCoin - consumedCoin
	if _, err := tx.Exec(query, totalCoin, user.ID); err != nil {
		return errorResponse(c, http.StatusInternalServerError, err)
	}

	err = tx.Commit()
	if err != nil {
		return errorResponse(c, http.StatusInternalServerError, err)
	}

	return successResponse(c, &DrawGachaResponse{
		Presents: presents,
	})
}

type DrawGachaRequest struct {
	ViewerID     string `json:"viewerId"`
	OneTimeToken string `json:"oneTimeToken"`
}

type DrawGachaResponse struct {
	Presents []*UserPresent `json:"presents"`
}

// listPresent プレゼント一覧
// GET /user/{userID}/present/index/{n}
func (h *Handler) listPresent(c echo.Context) error {
	n, err := strconv.Atoi(c.Param("n"))
	if err != nil {
		return errorResponse(c, http.StatusBadRequest, fmt.Errorf("invalid index number (n) parameter"))
	}
	if n == 0 {
		return errorResponse(c, http.StatusBadRequest, fmt.Errorf("index number (n) should be more than or equal to 1"))
	}

	userID, err := getUserID(c)
	if err != nil {
		return errorResponse(c, http.StatusBadRequest, fmt.Errorf("invalid userID parameter"))
	}

	offset := PresentCountPerPage * (n - 1)
	// ユーザーIDに基づいて適切なDBを選択
	db := h.getDBForUserID(userID)

	presentList := []*UserPresent{}
	query := `
	SELECT * FROM user_presents 
	WHERE user_id = ? AND deleted_at IS NULL
	ORDER BY created_at DESC, id
	LIMIT ? OFFSET ?`
	if err = db.Select(&presentList, query, userID, PresentCountPerPage, offset); err != nil {
		return errorResponse(c, http.StatusInternalServerError, err)
	}

	var presentCount int
	if err = db.Get(&presentCount, "SELECT COUNT(*) FROM user_presents WHERE user_id = ? AND deleted_at IS NULL", userID); err != nil {
		return errorResponse(c, http.StatusInternalServerError, err)
	}

	isNext := false
	if presentCount > (offset + PresentCountPerPage) {
		isNext = true
	}

	return successResponse(c, &ListPresentResponse{
		Presents: presentList,
		IsNext:   isNext,
	})
}

type ListPresentResponse struct {
	Presents []*UserPresent `json:"presents"`
	IsNext   bool           `json:"isNext"`
}

// receivePresent プレゼント受け取り
// POST /user/{userID}/present/receive
func (h *Handler) receivePresent(c echo.Context) error {
	defer c.Request().Body.Close()
	req := new(ReceivePresentRequest)
	if err := parseRequestBody(c, req); err != nil {
		return errorResponse(c, http.StatusBadRequest, err)
	}

	userID, err := getUserID(c)
	if err != nil {
		return errorResponse(c, http.StatusBadRequest, err)
	}

	requestAt, err := getRequestTime(c)
	if err != nil {
		return errorResponse(c, http.StatusInternalServerError, ErrGetRequestTime)
	}

	if len(req.PresentIDs) == 0 {
		return errorResponse(c, http.StatusUnprocessableEntity, fmt.Errorf("presentIds is empty"))
	}

	if err = h.checkViewerID(userID, req.ViewerID); err != nil {
		if err == ErrUserDeviceNotFound {
			return errorResponse(c, http.StatusNotFound, err)
		}
		return errorResponse(c, http.StatusInternalServerError, err)
	}

	// ユーザーIDに基づいて適切なDBを選択
	db := h.getDBForUserID(userID)

	// 未取得のプレゼント取得
	query := "SELECT * FROM user_presents WHERE id IN (?) AND deleted_at IS NULL"
	query, params, err := sqlx.In(query, req.PresentIDs)
	if err != nil {
		return errorResponse(c, http.StatusBadRequest, err)
	}
	obtainPresent := []*UserPresent{}
	if err = db.Select(&obtainPresent, query, params...); err != nil {
		return errorResponse(c, http.StatusBadRequest, err)
	}

	if len(obtainPresent) == 0 {
		return successResponse(c, &ReceivePresentResponse{
			UpdatedResources: makeUpdatedResources(requestAt, nil, nil, nil, nil, nil, nil, []*UserPresent{}),
		})
	}

	tx, err := db.Beginx()
	if err != nil {
		return errorResponse(c, http.StatusInternalServerError, err)
	}
	defer tx.Rollback() //nolint:errcheck

	// プレゼントの削除処理をバッチ化
	presentIDs := make([]int64, len(obtainPresent))
	for i := range obtainPresent {
		if obtainPresent[i].DeletedAt != nil {
			return errorResponse(c, http.StatusInternalServerError, fmt.Errorf("received present"))
		}
		obtainPresent[i].UpdatedAt = requestAt
		obtainPresent[i].DeletedAt = &requestAt
		presentIDs[i] = obtainPresent[i].ID
	}

	// プレゼントを一括で削除済みにマーク
	query = "UPDATE user_presents SET deleted_at=?, updated_at=? WHERE id IN (?)"
	query, params, err = sqlx.In(query, requestAt, requestAt, presentIDs)
	if err != nil {
		return errorResponse(c, http.StatusInternalServerError, err)
	}
	_, err = tx.Exec(query, params...)
	if err != nil {
		return errorResponse(c, http.StatusInternalServerError, err)
	}

	// アイテム付与処理をバッチ化
	err = h.obtainItemsBatch(tx, obtainPresent, userID, requestAt)
	if err != nil {
		if err == ErrUserNotFound || err == ErrItemNotFound {
			return errorResponse(c, http.StatusNotFound, err)
		}
		if err == ErrInvalidItemType {
			return errorResponse(c, http.StatusBadRequest, err)
		}
		return errorResponse(c, http.StatusInternalServerError, err)
	}

	err = tx.Commit()
	if err != nil {
		return errorResponse(c, http.StatusInternalServerError, err)
	}

	return successResponse(c, &ReceivePresentResponse{
		UpdatedResources: makeUpdatedResources(requestAt, nil, nil, nil, nil, nil, nil, obtainPresent),
	})
}

type ReceivePresentRequest struct {
	ViewerID   string  `json:"viewerId"`
	PresentIDs []int64 `json:"presentIds"`
}

type ReceivePresentResponse struct {
	UpdatedResources *UpdatedResource `json:"updatedResources"`
}

// listItem アイテムリスト
// GET /user/{userID}/item
func (h *Handler) listItem(c echo.Context) error {
	userID, err := getUserID(c)
	if err != nil {
		return errorResponse(c, http.StatusBadRequest, err)
	}

	requestAt, err := getRequestTime(c)
	if err != nil {
		return errorResponse(c, http.StatusInternalServerError, ErrGetRequestTime)
	}

	// ユーザーIDに基づいて適切なDBを選択
	db := h.getDBForUserID(userID)

	user := new(User)
	query := "SELECT * FROM users WHERE id=?"
	if err = db.Get(user, query, userID); err != nil {
		if err == sql.ErrNoRows {
			return errorResponse(c, http.StatusNotFound, ErrUserNotFound)
		}
		return errorResponse(c, http.StatusInternalServerError, err)
	}

	itemList := []*UserItem{}
	query = "SELECT * FROM user_items WHERE user_id = ?"
	if err = db.Select(&itemList, query, userID); err != nil {
		return errorResponse(c, http.StatusInternalServerError, err)
	}

	cardList := make([]*UserCard, 0)
	query = "SELECT * FROM user_cards WHERE user_id=?"
	if err = db.Select(&cardList, query, userID); err != nil {
		return errorResponse(c, http.StatusInternalServerError, err)
	}

	// アイテムの強化に使うためのワンタイムトークンを発行
	query = "UPDATE user_one_time_tokens SET deleted_at=? WHERE user_id=? AND deleted_at IS NULL"
	if _, err = db.Exec(query, requestAt, userID); err != nil {
		return errorResponse(c, http.StatusInternalServerError, err)
	}
	tID, err := h.generateID()
	if err != nil {
		return errorResponse(c, http.StatusInternalServerError, err)
	}
	tk, err := generateUUID()
	if err != nil {
		return errorResponse(c, http.StatusInternalServerError, err)
	}
	token := &UserOneTimeToken{
		ID:        tID,
		UserID:    userID,
		Token:     tk,
		TokenType: 2,
		CreatedAt: requestAt,
		UpdatedAt: requestAt,
		ExpiredAt: requestAt + 600,
	}
	query = "INSERT INTO user_one_time_tokens(id, user_id, token, token_type, created_at, updated_at, expired_at) VALUES (?, ?, ?, ?, ?, ?, ?)"
	if _, err = h.DB.Exec(query, token.ID, token.UserID, token.Token, token.TokenType, token.CreatedAt, token.UpdatedAt, token.ExpiredAt); err != nil {
		return errorResponse(c, http.StatusInternalServerError, err)
	}

	// キャッシュにも保存
	h.TokenCache.SetToken(token.Token, token.UserID, token.TokenType, token.ExpiredAt, token.CreatedAt)

	return successResponse(c, &ListItemResponse{
		OneTimeToken: token.Token,
		Items:        itemList,
		User:         user,
		Cards:        cardList,
	})
}

type ListItemResponse struct {
	OneTimeToken string      `json:"oneTimeToken"`
	User         *User       `json:"user"`
	Items        []*UserItem `json:"items"`
	Cards        []*UserCard `json:"cards"`
}

// addExpToCard 装備強化
// POST /user/{userID}/card/addexp/{cardID}
func (h *Handler) addExpToCard(c echo.Context) error {
	cardID, err := strconv.ParseInt(c.Param("cardID"), 10, 64)
	if err != nil {
		return errorResponse(c, http.StatusBadRequest, err)
	}

	userID, err := getUserID(c)
	if err != nil {
		return errorResponse(c, http.StatusBadRequest, err)
	}

	// read body
	defer c.Request().Body.Close()
	req := new(AddExpToCardRequest)
	if err := parseRequestBody(c, req); err != nil {
		return errorResponse(c, http.StatusBadRequest, err)
	}

	requestAt, err := getRequestTime(c)
	if err != nil {
		return errorResponse(c, http.StatusInternalServerError, ErrGetRequestTime)
	}

	if err = h.checkOneTimeToken(userID, req.OneTimeToken, 2, requestAt); err != nil {
		if err == ErrInvalidToken {
			return errorResponse(c, http.StatusBadRequest, err)
		}
		return errorResponse(c, http.StatusInternalServerError, err)
	}

	if err = h.checkViewerID(userID, req.ViewerID); err != nil {
		if err == ErrUserDeviceNotFound {
			return errorResponse(c, http.StatusNotFound, err)
		}
		return errorResponse(c, http.StatusInternalServerError, err)
	}

	card := new(TargetUserCardData)
	query := `
	SELECT uc.id , uc.user_id , uc.card_id , uc.amount_per_sec , uc.level, uc.total_exp, im.amount_per_sec as 'base_amount_per_sec', im.max_level , im.max_amount_per_sec , im.base_exp_per_level
	FROM user_cards as uc
	INNER JOIN item_masters as im ON uc.card_id = im.id
	WHERE uc.id = ? AND uc.user_id=?
	`
	if err = h.getDBForUserID(userID).Get(card, query, cardID, userID); err != nil {
		if err == sql.ErrNoRows {
			return errorResponse(c, http.StatusNotFound, err)
		}
		return errorResponse(c, http.StatusInternalServerError, err)
	}

	if card.Level == card.MaxLevel {
		return errorResponse(c, http.StatusBadRequest, fmt.Errorf("target card is max level"))
	}

	items := make([]*ConsumeUserItemData, 0)
	query = `
	SELECT ui.id, ui.user_id, ui.item_id, ui.item_type, ui.amount, ui.created_at, ui.updated_at, im.gained_exp
	FROM user_items as ui
	INNER JOIN item_masters as im ON ui.item_id = im.id
	WHERE ui.item_type = 3 AND ui.id=? AND ui.user_id=?
	`
	for _, v := range req.Items {
		item := new(ConsumeUserItemData)
		if err = h.getDBForUserID(userID).Get(item, query, v.ID, userID); err != nil {
			if err == sql.ErrNoRows {
				return errorResponse(c, http.StatusNotFound, err)
			}
			return errorResponse(c, http.StatusInternalServerError, err)
		}

		if v.Amount > item.Amount {
			return errorResponse(c, http.StatusBadRequest, fmt.Errorf("item not enough"))
		}
		item.ConsumeAmount = v.Amount
		items = append(items, item)
	}

	for _, v := range items {
		card.TotalExp += v.GainedExp * v.ConsumeAmount
	}

	// lv up判定(lv upしたら生産性を加算)
	for {
		nextLvThreshold := int(float64(card.BaseExpPerLevel) * math.Pow(1.2, float64(card.Level-1)))
		if nextLvThreshold > card.TotalExp {
			break
		}

		// lv up処理
		card.Level += 1
		card.AmountPerSec += (card.MaxAmountPerSec - card.BaseAmountPerSec) / (card.MaxLevel - 1)
	}

	// ユーザーIDに基づいて適切なDBを選択
	db := h.getDBForUserID(userID)

	tx, err := db.Beginx()
	if err != nil {
		return errorResponse(c, http.StatusInternalServerError, err)
	}

	defer tx.Rollback() //nolint:errcheck

	query = "UPDATE user_cards SET amount_per_sec=?, level=?, total_exp=?, updated_at=? WHERE id=?"
	if _, err = tx.Exec(query, card.AmountPerSec, card.Level, card.TotalExp, requestAt, card.ID); err != nil {
		return errorResponse(c, http.StatusInternalServerError, err)
	}

	query = "UPDATE user_items SET amount=?, updated_at=? WHERE id=?"
	for _, v := range items {
		if _, err = tx.Exec(query, v.Amount-v.ConsumeAmount, requestAt, v.ID); err != nil {
			return errorResponse(c, http.StatusInternalServerError, err)
		}
	}

	resultCard := new(UserCard)
	query = "SELECT * FROM user_cards WHERE id=?"
	if err = tx.Get(resultCard, query, card.ID); err != nil {
		if err == sql.ErrNoRows {
			return errorResponse(c, http.StatusNotFound, fmt.Errorf("not found card"))
		}
		return errorResponse(c, http.StatusInternalServerError, err)
	}
	resultItems := make([]*UserItem, 0)
	for _, v := range items {
		resultItems = append(resultItems, &UserItem{
			ID:        v.ID,
			UserID:    v.UserID,
			ItemID:    v.ItemID,
			ItemType:  v.ItemType,
			Amount:    v.Amount - v.ConsumeAmount,
			CreatedAt: v.CreatedAt,
			UpdatedAt: requestAt,
		})
	}

	err = tx.Commit()
	if err != nil {
		return errorResponse(c, http.StatusInternalServerError, err)
	}

	return successResponse(c, &AddExpToCardResponse{
		UpdatedResources: makeUpdatedResources(requestAt, nil, nil, []*UserCard{resultCard}, nil, resultItems, nil, nil),
	})
}

type AddExpToCardRequest struct {
	ViewerID     string         `json:"viewerId"`
	OneTimeToken string         `json:"oneTimeToken"`
	Items        []*ConsumeItem `json:"items"`
}

type AddExpToCardResponse struct {
	UpdatedResources *UpdatedResource `json:"updatedResources"`
}

type ConsumeItem struct {
	ID     int64 `json:"id"`
	Amount int   `json:"amount"`
}

type ConsumeUserItemData struct {
	ID        int64 `db:"id"`
	UserID    int64 `db:"user_id"`
	ItemID    int64 `db:"item_id"`
	ItemType  int   `db:"item_type"`
	Amount    int   `db:"amount"`
	CreatedAt int64 `db:"created_at"`
	UpdatedAt int64 `db:"updated_at"`
	GainedExp int   `db:"gained_exp"`

	ConsumeAmount int // 消費量
}

type TargetUserCardData struct {
	ID               int64 `db:"id"`
	UserID           int64 `db:"user_id"`
	CardID           int64 `db:"card_id"`
	AmountPerSec     int   `db:"amount_per_sec"`
	Level            int   `db:"level"`
	TotalExp         int   `db:"total_exp"`
	BaseAmountPerSec int   `db:"base_amount_per_sec"`
	MaxLevel         int   `db:"max_level"`
	MaxAmountPerSec  int   `db:"max_amount_per_sec"`
	BaseExpPerLevel  int   `db:"base_exp_per_level"`
}

// updateDeck 装備変更
// POST /user/{userID}/card
func (h *Handler) updateDeck(c echo.Context) error {
	userID, err := getUserID(c)
	if err != nil {
		return errorResponse(c, http.StatusBadRequest, err)
	}

	defer c.Request().Body.Close()
	req := new(UpdateDeckRequest)
	if err := parseRequestBody(c, req); err != nil {
		return errorResponse(c, http.StatusBadRequest, err)
	}

	if len(req.CardIDs) != DeckCardNumber {
		return errorResponse(c, http.StatusBadRequest, fmt.Errorf("invalid number of cards"))
	}

	requestAt, err := getRequestTime(c)
	if err != nil {
		return errorResponse(c, http.StatusInternalServerError, ErrGetRequestTime)
	}

	if err = h.checkViewerID(userID, req.ViewerID); err != nil {
		if err == ErrUserDeviceNotFound {
			return errorResponse(c, http.StatusNotFound, err)
		}
		return errorResponse(c, http.StatusInternalServerError, err)
	}

	// ユーザーIDに基づいて適切なDBを選択
	db := h.getDBForUserID(userID)

	query := "SELECT * FROM user_cards WHERE id IN (?)"
	query, params, err := sqlx.In(query, req.CardIDs)
	if err != nil {
		return errorResponse(c, http.StatusBadRequest, err)
	}
	cards := make([]*UserCard, 0)
	if err = db.Select(&cards, query, params...); err != nil {
		return errorResponse(c, http.StatusInternalServerError, err)
	}
	if len(cards) != DeckCardNumber {
		return errorResponse(c, http.StatusBadRequest, fmt.Errorf("invalid card ids"))
	}

	tx, err := db.Beginx()
	if err != nil {
		return errorResponse(c, http.StatusInternalServerError, err)
	}

	defer tx.Rollback() //nolint:errcheck

	query = "UPDATE user_decks SET updated_at=?, deleted_at=? WHERE user_id=? AND deleted_at IS NULL"
	if _, err = tx.Exec(query, requestAt, requestAt, userID); err != nil {
		return errorResponse(c, http.StatusInternalServerError, err)
	}

	udID, err := h.generateID()
	if err != nil {
		return errorResponse(c, http.StatusInternalServerError, err)
	}
	newDeck := &UserDeck{
		ID:        udID,
		UserID:    userID,
		CardID1:   req.CardIDs[0],
		CardID2:   req.CardIDs[1],
		CardID3:   req.CardIDs[2],
		CreatedAt: requestAt,
		UpdatedAt: requestAt,
	}
	query = "INSERT INTO user_decks(id, user_id, user_card_id_1, user_card_id_2, user_card_id_3, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)"
	if _, err := tx.Exec(query, newDeck.ID, newDeck.UserID, newDeck.CardID1, newDeck.CardID2, newDeck.CardID3, newDeck.CreatedAt, newDeck.UpdatedAt); err != nil {
		return errorResponse(c, http.StatusInternalServerError, err)
	}

	err = tx.Commit()
	if err != nil {
		return errorResponse(c, http.StatusInternalServerError, err)
	}

	return successResponse(c, &UpdateDeckResponse{
		UpdatedResources: makeUpdatedResources(requestAt, nil, nil, nil, []*UserDeck{newDeck}, nil, nil, nil),
	})
}

type UpdateDeckRequest struct {
	ViewerID string  `json:"viewerId"`
	CardIDs  []int64 `json:"cardIds"`
}

type UpdateDeckResponse struct {
	UpdatedResources *UpdatedResource `json:"updatedResources"`
}

// reward ゲーム報酬受取
// POST /user/{userID}/reward
func (h *Handler) reward(c echo.Context) error {
	userID, err := getUserID(c)
	if err != nil {
		return errorResponse(c, http.StatusBadRequest, err)
	}

	defer c.Request().Body.Close()
	req := new(RewardRequest)
	if err := parseRequestBody(c, req); err != nil {
		return errorResponse(c, http.StatusBadRequest, err)
	}

	requestAt, err := getRequestTime(c)
	if err != nil {
		return errorResponse(c, http.StatusInternalServerError, ErrGetRequestTime)
	}

	if err = h.checkViewerID(userID, req.ViewerID); err != nil {
		if err == ErrUserDeviceNotFound {
			return errorResponse(c, http.StatusNotFound, err)
		}
		return errorResponse(c, http.StatusInternalServerError, err)
	}

	// ユーザーIDに基づいて適切なDBを選択
	db := h.getDBForUserID(userID)

	user := new(User)
	query := "SELECT * FROM users WHERE id=?"
	if err = db.Get(user, query, userID); err != nil {
		if err == sql.ErrNoRows {
			return errorResponse(c, http.StatusNotFound, ErrUserNotFound)
		}
		return errorResponse(c, http.StatusInternalServerError, err)
	}

	deck := new(UserDeck)
	query = "SELECT * FROM user_decks WHERE user_id=? AND deleted_at IS NULL"
	if err = db.Get(deck, query, userID); err != nil {
		if err == sql.ErrNoRows {
			return errorResponse(c, http.StatusNotFound, err)
		}
		return errorResponse(c, http.StatusInternalServerError, err)
	}

	cards := make([]*UserCard, 0)
	query = "SELECT * FROM user_cards WHERE id IN (?, ?, ?)"
	if err = db.Select(&cards, query, deck.CardID1, deck.CardID2, deck.CardID3); err != nil {
		return errorResponse(c, http.StatusInternalServerError, err)
	}
	if len(cards) != 3 {
		return errorResponse(c, http.StatusBadRequest, fmt.Errorf("invalid cards length"))
	}

	pastTime := requestAt - user.LastGetRewardAt
	getCoin := int(pastTime) * (cards[0].AmountPerSec + cards[1].AmountPerSec + cards[2].AmountPerSec)

	user.IsuCoin += int64(getCoin)
	user.LastGetRewardAt = requestAt

	query = "UPDATE users SET isu_coin=?, last_getreward_at=? WHERE id=?"
	if _, err = db.Exec(query, user.IsuCoin, user.LastGetRewardAt, user.ID); err != nil {
		return errorResponse(c, http.StatusInternalServerError, err)
	}

	return successResponse(c, &RewardResponse{
		UpdatedResources: makeUpdatedResources(requestAt, user, nil, nil, nil, nil, nil, nil),
	})
}

type RewardRequest struct {
	ViewerID string `json:"viewerId"`
}

type RewardResponse struct {
	UpdatedResources *UpdatedResource `json:"updatedResources"`
}

// home ホーム取得
// GET /user/{userID}/home
func (h *Handler) home(c echo.Context) error {
	userID, err := getUserID(c)
	if err != nil {
		return errorResponse(c, http.StatusBadRequest, err)
	}

	requestAt, err := getRequestTime(c)
	if err != nil {
		return errorResponse(c, http.StatusInternalServerError, ErrGetRequestTime)
	}

	// ユーザーIDに基づいて適切なDBを選択
	db := h.getDBForUserID(userID)

	deck := new(UserDeck)
	query := "SELECT * FROM user_decks WHERE user_id=? AND deleted_at IS NULL"
	if err = db.Get(deck, query, userID); err != nil {
		if err != sql.ErrNoRows {
			return errorResponse(c, http.StatusInternalServerError, err)
		}
		deck = nil
	}

	cards := make([]*UserCard, 0)
	if deck != nil {
		cardIds := []int64{deck.CardID1, deck.CardID2, deck.CardID3}
		query, params, err := sqlx.In("SELECT * FROM user_cards WHERE id IN (?)", cardIds)
		if err != nil {
			return errorResponse(c, http.StatusInternalServerError, err)
		}
		if err = db.Select(&cards, query, params...); err != nil {
			return errorResponse(c, http.StatusInternalServerError, err)
		}
	}
	totalAmountPerSec := 0
	for _, v := range cards {
		totalAmountPerSec += v.AmountPerSec
	}

	user := new(User)
	query = "SELECT * FROM users WHERE id=?"
	if err = db.Get(user, query, userID); err != nil {
		if err == sql.ErrNoRows {
			return errorResponse(c, http.StatusNotFound, ErrUserNotFound)
		}
		return errorResponse(c, http.StatusInternalServerError, err)
	}
	pastTime := requestAt - user.LastGetRewardAt

	return successResponse(c, &HomeResponse{
		Now:               requestAt,
		User:              user,
		Deck:              deck,
		TotalAmountPerSec: totalAmountPerSec,
		PastTime:          pastTime,
	})
}

type HomeResponse struct {
	Now               int64     `json:"now"`
	User              *User     `json:"user"`
	Deck              *UserDeck `json:"deck,omitempty"`
	TotalAmountPerSec int       `json:"totalAmountPerSec"`
	PastTime          int64     `json:"pastTime"` // 経過時間を秒単位で
}

// //////////////////////////////////////
// util

// health ヘルスチェック
func (h *Handler) health(c echo.Context) error {
	return c.String(http.StatusOK, "OK")
}

// errorResponse エラーレスポンス
func errorResponse(c echo.Context, statusCode int, err error) error {
	c.Logger().Errorf("status=%d, err=%+v", statusCode, errors.WithStack(err))

	return c.JSON(statusCode, struct {
		StatusCode int    `json:"status_code"`
		Message    string `json:"message"`
	}{
		StatusCode: statusCode,
		Message:    err.Error(),
	})
}

// successResponse 成功時のレスポンス
func successResponse(c echo.Context, v interface{}) error {
	return c.JSON(http.StatusOK, v)
}

// noContentResponse
func noContentResponse(c echo.Context, status int) error {
	return c.NoContent(status)
}

// generateID ユニークなIDを生成する
func (h *Handler) generateID() (int64, error) {
	id := snowflakeNode.Generate()

	return id.Int64(), nil
}

// generateUUID UUIDの生成
func generateUUID() (string, error) {
	id, err := uuid.NewRandom()
	if err != nil {
		return "", err
	}

	return id.String(), nil
}

// getUserID path paramからuserIDを取得する
func getUserID(c echo.Context) (int64, error) {
	return strconv.ParseInt(c.Param("userID"), 10, 64)
}

// getEnv 環境変数から値を取得する
func getEnv(key, defaultVal string) string {
	if v := os.Getenv(key); v == "" {
		return defaultVal
	} else {
		return v
	}
}

// getDBForUserID ユーザーIDに基づいて適切なDBを選択する
func (h *Handler) getDBForUserID(userID int64) *sqlx.DB {
	if len(h.DBs) == 0 {
		return h.DB
	}

	// ユーザーIDに基づいてシャーディング
	// snowflake IDの場合、上位ビットはタイムスタンプなので、下位ビットを使用する
	index := int(userID>>23) % len(h.DBs)
	return h.DBs[index]
}

// parseRequestBody リクエストボディをパースする
func parseRequestBody(c echo.Context, dist interface{}) error {
	buf, err := io.ReadAll(c.Request().Body)
	if err != nil {
		return ErrInvalidRequestBody
	}
	if err = json.Unmarshal(buf, &dist); err != nil {
		return ErrInvalidRequestBody
	}
	return nil
}

type UpdatedResource struct {
	Now  int64 `json:"now"`
	User *User `json:"user,omitempty"`

	UserDevice       *UserDevice       `json:"userDevice,omitempty"`
	UserCards        []*UserCard       `json:"userCards,omitempty"`
	UserDecks        []*UserDeck       `json:"userDecks,omitempty"`
	UserItems        []*UserItem       `json:"userItems,omitempty"`
	UserLoginBonuses []*UserLoginBonus `json:"userLoginBonuses,omitempty"`
	UserPresents     []*UserPresent    `json:"userPresents,omitempty"`
}

// makeUpdateResources 更新リソース返却用のオブジェクトを作成する
func makeUpdatedResources(
	requestAt int64,
	user *User,
	userDevice *UserDevice,
	userCards []*UserCard,
	userDecks []*UserDeck,
	userItems []*UserItem,
	userLoginBonuses []*UserLoginBonus,
	userPresents []*UserPresent,
) *UpdatedResource {
	return &UpdatedResource{
		Now:              requestAt,
		User:             user,
		UserDevice:       userDevice,
		UserCards:        userCards,
		UserItems:        userItems,
		UserDecks:        userDecks,
		UserLoginBonuses: userLoginBonuses,
		UserPresents:     userPresents,
	}
}

// //////////////////////////////////////
// entity

type User struct {
	ID              int64  `json:"id" db:"id"`
	IsuCoin         int64  `json:"isuCoin" db:"isu_coin"`
	LastGetRewardAt int64  `json:"lastGetRewardAt" db:"last_getreward_at"`
	LastActivatedAt int64  `json:"lastActivatedAt" db:"last_activated_at"`
	RegisteredAt    int64  `json:"registeredAt" db:"registered_at"`
	CreatedAt       int64  `json:"createdAt" db:"created_at"`
	UpdatedAt       int64  `json:"updatedAt" db:"updated_at"`
	DeletedAt       *int64 `json:"deletedAt,omitempty" db:"deleted_at"`
}

type UserDevice struct {
	ID           int64  `json:"id" db:"id"`
	UserID       int64  `json:"userId" db:"user_id"`
	PlatformID   string `json:"platformId" db:"platform_id"`
	PlatformType int    `json:"platformType" db:"platform_type"`
	CreatedAt    int64  `json:"createdAt" db:"created_at"`
	UpdatedAt    int64  `json:"updatedAt" db:"updated_at"`
	DeletedAt    *int64 `json:"deletedAt,omitempty" db:"deleted_at"`
}

type UserBan struct {
	ID        int64  `db:"id"`
	UserID    int64  `db:"user_id"`
	CreatedAt int64  `db:"created_at"`
	UpdatedAt int64  `db:"updated_at"`
	DeletedAt *int64 `db:"deleted_at"`
}

type UserCard struct {
	ID           int64  `json:"id" db:"id"`
	UserID       int64  `json:"userId" db:"user_id"`
	CardID       int64  `json:"cardId" db:"card_id"`
	AmountPerSec int    `json:"amountPerSec" db:"amount_per_sec"`
	Level        int    `json:"level" db:"level"`
	TotalExp     int64  `json:"totalExp" db:"total_exp"`
	CreatedAt    int64  `json:"createdAt" db:"created_at"`
	UpdatedAt    int64  `json:"updatedAt" db:"updated_at"`
	DeletedAt    *int64 `json:"deletedAt,omitempty" db:"deleted_at"`
}

type UserDeck struct {
	ID        int64  `json:"id" db:"id"`
	UserID    int64  `json:"userId" db:"user_id"`
	CardID1   int64  `json:"cardId1" db:"user_card_id_1"`
	CardID2   int64  `json:"cardId2" db:"user_card_id_2"`
	CardID3   int64  `json:"cardId3" db:"user_card_id_3"`
	CreatedAt int64  `json:"createdAt" db:"created_at"`
	UpdatedAt int64  `json:"updatedAt" db:"updated_at"`
	DeletedAt *int64 `json:"deletedAt,omitempty" db:"deleted_at"`
}

type UserItem struct {
	ID        int64  `json:"id" db:"id"`
	UserID    int64  `json:"userId" db:"user_id"`
	ItemType  int    `json:"itemType" db:"item_type"`
	ItemID    int64  `json:"itemId" db:"item_id"`
	Amount    int    `json:"amount" db:"amount"`
	CreatedAt int64  `json:"createdAt" db:"created_at"`
	UpdatedAt int64  `json:"updatedAt" db:"updated_at"`
	DeletedAt *int64 `json:"deletedAt,omitempty" db:"deleted_at"`
}

type UserLoginBonus struct {
	ID                 int64  `json:"id" db:"id"`
	UserID             int64  `json:"userId" db:"user_id"`
	LoginBonusID       int64  `json:"loginBonusId" db:"login_bonus_id"`
	LastRewardSequence int    `json:"lastRewardSequence" db:"last_reward_sequence"`
	LoopCount          int    `json:"loopCount" db:"loop_count"`
	CreatedAt          int64  `json:"createdAt" db:"created_at"`
	UpdatedAt          int64  `json:"updatedAt" db:"updated_at"`
	DeletedAt          *int64 `json:"deletedAt,omitempty" db:"deleted_at"`
}

type UserPresent struct {
	ID             int64  `json:"id" db:"id"`
	UserID         int64  `json:"userId" db:"user_id"`
	SentAt         int64  `json:"sentAt" db:"sent_at"`
	ItemType       int    `json:"itemType" db:"item_type"`
	ItemID         int64  `json:"itemId" db:"item_id"`
	Amount         int    `json:"amount" db:"amount"`
	PresentMessage string `json:"presentMessage" db:"present_message"`
	CreatedAt      int64  `json:"createdAt" db:"created_at"`
	UpdatedAt      int64  `json:"updatedAt" db:"updated_at"`
	DeletedAt      *int64 `json:"deletedAt,omitempty" db:"deleted_at"`
}

type UserPresentAllReceivedHistory struct {
	ID           int64  `json:"id" db:"id"`
	UserID       int64  `json:"userId" db:"user_id"`
	PresentAllID int64  `json:"presentAllId" db:"present_all_id"`
	ReceivedAt   int64  `json:"receivedAt" db:"received_at"`
	CreatedAt    int64  `json:"createdAt" db:"created_at"`
	UpdatedAt    int64  `json:"updatedAt" db:"updated_at"`
	DeletedAt    *int64 `json:"deletedAt,omitempty" db:"deleted_at"`
}

type Session struct {
	ID        int64  `json:"id" db:"id"`
	UserID    int64  `json:"userId" db:"user_id"`
	SessionID string `json:"sessionId" db:"session_id"`
	ExpiredAt int64  `json:"expiredAt" db:"expired_at"`
	CreatedAt int64  `json:"createdAt" db:"created_at"`
	UpdatedAt int64  `json:"updatedAt" db:"updated_at"`
	DeletedAt *int64 `json:"deletedAt,omitempty" db:"deleted_at"`
}

type UserOneTimeToken struct {
	ID        int64  `json:"id" db:"id"`
	UserID    int64  `json:"userId" db:"user_id"`
	Token     string `json:"token" db:"token"`
	TokenType int    `json:"tokenType" db:"token_type"`
	ExpiredAt int64  `json:"expiredAt" db:"expired_at"`
	CreatedAt int64  `json:"createdAt" db:"created_at"`
	UpdatedAt int64  `json:"updatedAt" db:"updated_at"`
	DeletedAt *int64 `json:"deletedAt,omitempty" db:"deleted_at"`
}

// //////////////////////////////////////
// master entity

type GachaMaster struct {
	ID           int64  `json:"id" db:"id"`
	Name         string `json:"name" db:"name"`
	StartAt      int64  `json:"startAt" db:"start_at"`
	EndAt        int64  `json:"endAt" db:"end_at"`
	DisplayOrder int    `json:"displayOrder" db:"display_order"`
	CreatedAt    int64  `json:"createdAt" db:"created_at"`
}

type GachaItemMaster struct {
	ID        int64 `json:"id" db:"id"`
	GachaID   int64 `json:"gachaId" db:"gacha_id"`
	ItemType  int   `json:"itemType" db:"item_type"`
	ItemID    int64 `json:"itemId" db:"item_id"`
	Amount    int   `json:"amount" db:"amount"`
	Weight    int   `json:"weight" db:"weight"`
	CreatedAt int64 `json:"createdAt" db:"created_at"`
}

type ItemMaster struct {
	ID              int64  `json:"id" db:"id"`
	ItemType        int    `json:"itemType" db:"item_type"`
	Name            string `json:"name" db:"name"`
	Description     string `json:"description" db:"description"`
	AmountPerSec    *int   `json:"amountPerSec" db:"amount_per_sec"`
	MaxLevel        *int   `json:"maxLevel" db:"max_level"`
	MaxAmountPerSec *int   `json:"maxAmountPerSec" db:"max_amount_per_sec"`
	BaseExpPerLevel *int   `json:"baseExpPerLevel" db:"base_exp_per_level"`
	GainedExp       *int   `json:"gainedExp" db:"gained_exp"`
	ShorteningMin   *int64 `json:"shorteningMin" db:"shortening_min"`
	// CreatedAt       int64 `json:"createdAt"`
}

type LoginBonusMaster struct {
	ID          int64 `json:"id" db:"id"`
	StartAt     int64 `json:"startAt" db:"start_at"`
	EndAt       int64 `json:"endAt" db:"end_at"`
	ColumnCount int   `json:"columnCount" db:"column_count"`
	Looped      bool  `json:"looped" db:"looped"`
	CreatedAt   int64 `json:"createdAt" db:"created_at"`
}

type LoginBonusRewardMaster struct {
	ID             int64 `json:"id" db:"id"`
	LoginBonusID   int64 `json:"loginBonusId" db:"login_bonus_id"`
	RewardSequence int   `json:"rewardSequence" db:"reward_sequence"`
	ItemType       int   `json:"itemType" db:"item_type"`
	ItemID         int64 `json:"itemId" db:"item_id"`
	Amount         int64 `json:"amount" db:"amount"`
	CreatedAt      int64 `json:"createdAt" db:"created_at"`
}

type PresentAllMaster struct {
	ID                int64  `json:"id" db:"id"`
	RegisteredStartAt int64  `json:"registeredStartAt" db:"registered_start_at"`
	RegisteredEndAt   int64  `json:"registeredEndAt" db:"registered_end_at"`
	ItemType          int    `json:"itemType" db:"item_type"`
	ItemID            int64  `json:"itemId" db:"item_id"`
	Amount            int64  `json:"amount" db:"amount"`
	PresentMessage    string `json:"presentMessage" db:"present_message"`
	CreatedAt         int64  `json:"createdAt" db:"created_at"`
}

type VersionMaster struct {
	ID            int64  `json:"id" db:"id"`
	Status        int    `json:"status" db:"status"`
	MasterVersion string `json:"masterVersion" db:"master_version"`
}
