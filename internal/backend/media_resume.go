package backend

import (
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

func (a *App) handleUserItemsResume(w http.ResponseWriter, r *http.Request) {
	reqCtx := requestContextFrom(r.Context())
	// 非管理员用户：从本地 WatchStore 服务继续观看列表
	if a.WatchStore != nil && reqCtx != nil && reqCtx.ProxyUser != nil && reqCtx.ProxyUser.Role != "admin" {
		a.handleLocalResume(w, r, reqCtx)
		return
	}
	query := cloneValues(r.URL.Query())
	parentID := firstQueryValue(query, "ParentId", "parentId", "parentid")
	if parentID != "" {
		resolved := a.resolveRouteID(parentID)
		if resolved == nil {
			writeJSON(w, http.StatusOK, map[string]any{"Items": []any{}, "TotalRecordCount": 0, "StartIndex": 0})
			return
		}
		if !a.requireServerAccess(w, r, resolved) {
			return
		}
		instances := a.collectAllowedInstances(requestContextFrom(r.Context()), resolved)
		originalIDs := map[string]struct{}{}
		for _, inst := range instances {
			originalIDs[inst.OriginalID] = struct{}{}
		}
		for _, inst := range instances {
			instQuery := cloneValues(query)
			instQuery.Set("ParentId", inst.OriginalID)
			instQuery.Set("UserId", inst.Client.clientUserID())
			instQuery.Del("parentId")
			instQuery.Del("parentid")
			payload, err := inst.Client.RequestJSON(r.Context(), requestContextFrom(r.Context()), a.Identity, http.MethodGet, "/Users/"+inst.Client.clientUserID()+"/Items/Resume", instQuery, nil)
			if err != nil {
				continue
			}
			filtered := filterSeriesItems(asItems(payload), originalIDs)
			if len(filtered) > 0 {
				a.rewriteItems(filtered, inst.ServerID, a.clientFacingUserIDFor(r))
				writeJSON(w, http.StatusOK, map[string]any{"Items": filtered, "TotalRecordCount": len(filtered), "StartIndex": 0})
				return
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"Items": []any{}, "TotalRecordCount": 0, "StartIndex": 0})
		return
	}
	results := a.fetchItemsAcrossUpstreams(r.Context(), requestContextFrom(r.Context()), "/Users/%s/Items/Resume", query, nil)
	writeJSON(w, http.StatusOK, a.mergedItemsPayload(results, a.clientFacingUserIDFor(r)))
}

// handleLocalResume 为非管理员用户从本地 WatchStore 提供继续观看列表。
// 它从上游服务器拉取媒体项元数据以构建完整响应。
func (a *App) handleLocalResume(w http.ResponseWriter, r *http.Request, reqCtx *RequestContext) {
	query := cloneValues(r.URL.Query())
	parentID := firstQueryValue(query, "ParentId", "parentId", "parentid")
	limit := 20
	if l, ok := queryInt(query, "Limit"); ok && l > 0 {
		limit = l
	}

	var items []WatchProgress
	var err error
	if parentID != "" {
		// 若指定了 ParentId，则精准获取属于该剧集的本地未看完单集
		all, allErr := a.WatchStore.GetResumableItems(reqCtx.ProxyUser.UserID)
		if allErr != nil {
			err = allErr
		} else {
			for _, item := range all {
				if item.SeriesVirtualID == parentID || item.SeriesOriginalID == parentID {
					items = append(items, item)
					if len(items) >= limit {
						break
					}
				}
			}
		}
	} else {
		items, err = a.WatchStore.GetResumeItems(reqCtx.ProxyUser.UserID, limit)
	}

	if err != nil || len(items) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"Items": []any{}, "TotalRecordCount": 0, "StartIndex": 0})
		return
	}

	enriched := a.enrichWatchItems(r, reqCtx, items)
	writeJSON(w, http.StatusOK, map[string]any{
		"Items":            toAnySlice(enriched),
		"TotalRecordCount": len(enriched),
		"StartIndex":       0,
	})
}

// enrichWatchItems 为 WatchProgress 列表从上游拉取最新元数据，
// 按服务器分组，优先通过 GET /Users/{userId}/Items?Ids=... 批量拉取（带 /Items 回退与单项兜底拉取），
// 并覆盖写入本地 UserData。当记录的服务器离线时，通过 IDStore 将项目重新映射至在线备用实例。
func (a *App) enrichWatchItems(r *http.Request, reqCtx *RequestContext, items []WatchProgress) []map[string]any {
	cfg := a.ConfigStore.Snapshot()

	// 按服务器 ID 分组，将离线服务器重新映射至在线备选实例
	type serverGroup struct {
		originalIDs []string
		watchItems  []WatchProgress
	}
	groups := map[string]*serverGroup{}
	for i := range items {
		serverID, originalID, ok := a.resolveWatchItemServer(&items[i])
		if !ok {
			continue // 所在实例均离线
		}
		items[i].ServerID = serverID
		items[i].OriginalItemID = originalID
		g, exists := groups[serverID]
		if !exists {
			g = &serverGroup{}
			groups[serverID] = g
		}
		g.originalIDs = append(g.originalIDs, originalID)
		g.watchItems = append(g.watchItems, items[i])
	}

	// 按服务器拉取元数据（当前所有分组均指向在线服务器）
	fetched := map[string]map[string]any{} // originalID → 媒体项元数据
	for serverID, g := range groups {
		client := a.Upstream.ClientByID(serverID)
		if client == nil || !client.IsOnline() {
			continue
		}
		// 过滤空 ID
		validIDs := make([]string, 0, len(g.originalIDs))
		for _, id := range g.originalIDs {
			if id != "" {
				validIDs = append(validIDs, id)
			}
		}
		if len(validIDs) == 0 {
			continue
		}

		q := url.Values{}
		q.Set("Ids", joinComma(validIDs))
		q.Set("Recursive", "true")
		q.Set("Fields", "BasicSyncInfo,CanDelete,PrimaryImageAspectRatio,Overview,DateCreated,MediaSources,Path,SortName,Studios,Taglines,Genres,CommunityRating,OfficialRating,CumulativeRunTimeTicks,RunTimeTicks,SeriesPrimaryImageTag,SeriesName,SeriesId,SeasonId,Chapters,ProviderIds")
		path := "/Items"
		if client.clientUserID() != "" {
			path = "/Users/" + client.clientUserID() + "/Items"
			q.Set("UserId", client.clientUserID())
		}
		payload, err := client.RequestJSON(r.Context(), reqCtx, a.Identity, http.MethodGet, path, q, nil)
		if err != nil && path != "/Items" {
			// 回退策略：若 /Users/{userId}/Items 失败（如测试模拟环境），尝试 /Items
			fallbackQ := cloneValues(q)
			fallbackQ.Del("UserId")
			payload, err = client.RequestJSON(r.Context(), reqCtx, a.Identity, http.MethodGet, "/Items", fallbackQ, nil)
		}
		if err != nil {
			if a.Logger != nil {
				a.Logger.Warnf("[Resume] 从上游 %s (%s) 批量拉取项目元数据失败: %v", client.Name, path, redactURLInError(err))
			}
		} else {
			for _, item := range asItems(payload) {
				if id, _ := item["Id"].(string); id != "" {
					fetched[id] = item
				}
			}
		}

		// 兜底策略：对批量查询未返回的有效 ID 逐个单项拉取
		for _, origID := range validIDs {
			if _, exists := fetched[origID]; exists {
				continue
			}
			singlePath := "/Items/" + origID
			singleQ := url.Values{}
			if client.clientUserID() != "" {
				singlePath = "/Users/" + client.clientUserID() + "/Items/" + origID
				singleQ.Set("UserId", client.clientUserID())
			}
			singleQ.Set("Fields", "BasicSyncInfo,CanDelete,PrimaryImageAspectRatio,Overview,DateCreated,MediaSources,Path,SortName,Studios,Taglines,Genres,CommunityRating,OfficialRating,CumulativeRunTimeTicks,RunTimeTicks,SeriesPrimaryImageTag,SeriesName,SeriesId,SeasonId,Chapters,ProviderIds")
			singlePayload, singleErr := client.RequestJSON(r.Context(), reqCtx, a.Identity, http.MethodGet, singlePath, singleQ, nil)
			if singleErr != nil {
				if a.Logger != nil {
					a.Logger.Warnf("[Resume] 从上游 %s (%s) 单项兜底拉取项目 %s 失败: %v", client.Name, singlePath, origID, redactURLInError(singleErr))
				}
				continue
			}
			if singleItem, ok := singlePayload.(map[string]any); ok {
				if id, _ := singleItem["Id"].(string); id != "" {
					fetched[id] = singleItem
				}
			}
		}
	}

	// 保持原顺序构建最终结果并覆盖本地 UserData
	var result []map[string]any
	for _, wp := range items {
		rawItem, ok := fetched[wp.OriginalItemID]
		if !ok {
			continue
		}
		// 在修改前浅拷贝 item map
		item := make(map[string]any, len(rawItem))
		for k, v := range rawItem {
			item[k] = v
		}
		// 将上游 ID 重写为代理虚拟 ID
		rewriteResponseIDs(item, wp.ServerID, a.IDStore, cfg.Server.ID, a.clientFacingUserIDFor(r))
		// 确保项目上存在 RunTimeTicks 运行时长
		rt, _ := numericInt64(item["RunTimeTicks"])
		if rt <= 0 && wp.RuntimeTicks > 0 {
			rt = wp.RuntimeTicks
			item["RunTimeTicks"] = rt
		}
		// 浅拷贝并覆盖本地 UserData
		ud, _ := item["UserData"].(map[string]any)
		if ud == nil {
			ud = map[string]any{}
		} else {
			clonedUD := make(map[string]any, len(ud))
			for k, v := range ud {
				clonedUD[k] = v
			}
			ud = clonedUD
		}
		ud["PlaybackPositionTicks"] = wp.PositionTicks
		ud["Played"] = wp.Played
		ud["IsFavorite"] = wp.IsFavorite
		if rt > 0 && wp.PositionTicks > 0 {
			pct := math.Round((float64(wp.PositionTicks)/float64(rt))*10000) / 100
			if pct > 100 {
				pct = 100
			}
			ud["PlayedPercentage"] = pct
		}
		item["UserData"] = ud
		result = append(result, item)
	}
	return result
}

// resolveWatchItemServer 返回用于拉取元数据的 serverID 和 originalItemID。
// 若记录的服务器离线，会尝试通过 IDStore 查找在线备用实例。
func (a *App) resolveWatchItemServer(wp *WatchProgress) (serverID string, originalItemID string, ok bool) {
	if wp.OriginalItemID != "" {
		client := a.Upstream.ClientByID(wp.ServerID)
		if client != nil && client.IsOnline() {
			return wp.ServerID, wp.OriginalItemID, true
		}
	}
	resolved := a.IDStore.ResolveVirtualID(wp.VirtualItemID)
	if resolved != nil {
		client := a.Upstream.ClientByID(resolved.ServerID)
		if client != nil && client.IsOnline() {
			return resolved.ServerID, resolved.OriginalID, true
		}
		for _, other := range resolved.OtherInstances {
			alt := a.Upstream.ClientByID(other.ServerID)
			if alt != nil && alt.IsOnline() {
				return other.ServerID, other.OriginalID, true
			}
		}
	}
	if wp.OriginalItemID != "" {
		client := a.Upstream.ClientByID(wp.ServerID)
		if client != nil && client.IsOnline() {
			return wp.ServerID, wp.OriginalItemID, true
		}
	}
	return "", "", false
}

// joinComma 用逗号连接字符串切片。
func joinComma(ss []string) string {
	if len(ss) == 0 {
		return ""
	}
	result := ss[0]
	for _, s := range ss[1:] {
		result += "," + s
	}
	return result
}

// queryInt 从 url.Values 解析整型查询参数。
func queryInt(values url.Values, key string) (int, bool) {
	s := strings.TrimSpace(values.Get(key))
	if s == "" {
		return 0, false
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, false
	}
	return n, true
}
