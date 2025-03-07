package resource

import (
	"encoding/base64"
	"fmt"
	"github.com/artpar/api2go"
	"github.com/artpar/go-guerrilla/backends"
	"github.com/artpar/go-guerrilla/mail"
	"github.com/artpar/go-imap"
	"github.com/artpar/go-imap/backend/backendutil"
	"github.com/buraksezer/olric"
	"github.com/daptin/daptin/server/database"
	"github.com/daptin/daptin/server/statementbuilder"
	"github.com/doug-martin/goqu/v9"
	"github.com/jmoiron/sqlx"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"io/ioutil"
	"os"
	"strings"
	"sync"
	"time"
)

type DbResource struct {
	model              api2go.Api2GoModel
	db                 sqlx.Ext
	Connection         database.DatabaseConnection
	tableInfo          *TableInfo
	Cruds              map[string]*DbResource
	ms                 *MiddlewareSet
	ActionHandlerMap   map[string]ActionPerformerInterface
	configStore        *ConfigStore
	contextCache       map[string]interface{}
	defaultGroups      []int64
	defaultRelations   map[string][]int64
	contextLock        sync.RWMutex
	OlricDb            *olric.Olric
	AssetFolderCache   map[string]map[string]*AssetFolderCache
	SubsiteFolderCache map[string]*AssetFolderCache
	MailSender         func(e *mail.Envelope, task backends.SelectTask) (backends.Result, error)
}

type AssetFolderCache struct {
	LocalSyncPath string
	Keyname       string
	CloudStore    CloudStore
}

func (afc *AssetFolderCache) GetFileByName(fileName string) (*os.File, error) {

	return os.Open(afc.LocalSyncPath + string(os.PathSeparator) + fileName)

}
func (afc *AssetFolderCache) DeleteFileByName(fileName string) error {

	return os.Remove(afc.LocalSyncPath + string(os.PathSeparator) + fileName)

}

func (afc *AssetFolderCache) GetPathContents(path string) ([]map[string]interface{}, error) {

	fileInfo, err := ioutil.ReadDir(afc.LocalSyncPath + string(os.PathSeparator) + path)
	if err != nil {
		return nil, err
	}

	//files, err := filepath.Glob(afc.LocalSyncPath + string(os.PathSeparator) + path + "*")
	//fmt.Println(files)
	var files []map[string]interface{}
	for _, file := range fileInfo {
		//files[i] = strings.Replace(file, afc.LocalSyncPath, "", 1)
		files = append(files, map[string]interface{}{
			"name":     file.Name(),
			"is_dir":   file.IsDir(),
			"mod_time": file.ModTime(),
			"size":     file.Size(),
		})
	}

	return files, err

}

func createDirIfNotExist(dir string) {
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		err = os.MkdirAll(dir, 0755)
		if err != nil {
			panic(err)
		}
	}
}

func (afc *AssetFolderCache) UploadFiles(files []interface{}) error {

	for i := range files {
		file := files[i].(map[string]interface{})
		contents, ok := file["file"]
		if !ok {
			contents = file["contents"]
		}
		if contents != nil {

			contentString, ok := contents.(string)
			if ok && len(contentString) > 4 {

				if strings.Index(contentString, ",") > -1 {
					contentString = strings.SplitN(contentString, ",", 2)[1]
				}
				fileBytes, e := base64.StdEncoding.DecodeString(contentString)
				if e != nil {
					continue
				}
				if file["name"] == nil {
					return errors.WithMessage(errors.New("file name cannot be null"), "File name is null")
				}
				filePath := string(os.PathSeparator)
				if file["path"] != nil {
					filePath = strings.Replace(file["path"].(string), "/", string(os.PathSeparator), -1) + string(os.PathSeparator)
				}
				localPath := afc.LocalSyncPath + string(os.PathSeparator) + filePath
				createDirIfNotExist(localPath)
				localFilePath := localPath + file["name"].(string)
				err := ioutil.WriteFile(localFilePath, fileBytes, os.ModePerm)
				CheckErr(err, "Failed to write data to local file store asset cache folder")
				if err != nil {
					return errors.WithMessage(err, "Failed to write data to local file store ")
				}
			}
		}
	}

	return nil

}

func NewDbResource(model api2go.Api2GoModel, db database.DatabaseConnection,
	ms *MiddlewareSet, cruds map[string]*DbResource, configStore *ConfigStore,
	olricDb *olric.Olric, tableInfo TableInfo) (*DbResource, error) {
	if OlricCache == nil {
		OlricCache, _ = olricDb.NewDMap("default-cache")
	}

	defaultgroupIds, err := GroupNamesToIds(db, tableInfo.DefaultGroups)
	if err != nil {
		return nil, err
	}
	defaultRelationsIds, err := RelationNamesToIds(db, tableInfo)
	if err != nil {
		return nil, err
	}

	//log.Printf("Columns [%v]: %v\n", model.GetName(), model.GetColumnNames())
	return &DbResource{
		model:              model,
		db:                 db,
		Connection:         db,
		ms:                 ms,
		configStore:        configStore,
		Cruds:              cruds,
		tableInfo:          &tableInfo,
		OlricDb:            olricDb,
		defaultGroups:      defaultgroupIds,
		defaultRelations:   defaultRelationsIds,
		contextCache:       make(map[string]interface{}),
		contextLock:        sync.RWMutex{},
		AssetFolderCache:   make(map[string]map[string]*AssetFolderCache),
		SubsiteFolderCache: make(map[string]*AssetFolderCache),
	}, nil
}

func RelationNamesToIds(db database.DatabaseConnection, tableInfo TableInfo) (map[string][]int64, error) {

	if len(tableInfo.DefaultRelations) == 0 {
		return map[string][]int64{}, nil
	}

	result := make(map[string][]int64)

	for relationName, values := range tableInfo.DefaultRelations {

		relation, found := tableInfo.GetRelationByName(relationName)
		if !found {
			log.Infof("Relation [%v] not found on table [%v] skipping default values", relationName, tableInfo.TableName)
			continue
		}

		typeName := relation.Subject

		if tableInfo.TableName == relation.Subject {
			typeName = relation.Object
		}

		query, args, err := statementbuilder.Squirrel.Select("id").From(typeName).Where(goqu.Ex{"reference_id": goqu.Op{"in": values}}).ToSQL()
		CheckErr(err, fmt.Sprintf("[165] failed to convert %v names to ids", relationName))
		query = db.Rebind(query)

		stmt1, err := db.Preparex(query)
		if err != nil {
			log.Errorf("[170] failed to prepare statment: %v", err)
			return map[string][]int64{}, fmt.Errorf("failed to prepare statment to convert usergroup name to ids for default usergroup")
		}
		defer func(stmt1 *sqlx.Stmt) {
			err := stmt1.Close()
			if err != nil {
				log.Errorf("failed to close prepared statement: %v", err)
			}
		}(stmt1)

		rows, err := stmt1.Queryx(args...)
		CheckErr(err, "[176] failed to query user-group names to ids")
		if err != nil {
			return nil, err
		}

		retInt := make([]int64, 0)

		for rows.Next() {
			//iVal, _ := strconv.ParseInt(val, 10, 64)
			var id int64
			err := rows.Scan(&id)
			if err != nil {
				log.Errorf("[185] failed to scan value after query: %v", err)
				return nil, err
			}
			retInt = append(retInt, id)
		}
		err = rows.Close()
		CheckErr(err, "[206] Failed to close rows after default group name conversation")

		result[relationName] = retInt

	}

	return result, nil

}

func GroupNamesToIds(db database.DatabaseConnection, groupsName []string) ([]int64, error) {

	if len(groupsName) == 0 {
		return []int64{}, nil
	}

	query, args, err := statementbuilder.Squirrel.Select("id").From("usergroup").Where(goqu.Ex{"name": goqu.Op{"in": groupsName}}).ToSQL()
	CheckErr(err, "[165] failed to convert usergroup names to ids")
	query = db.Rebind(query)

	stmt1, err := db.Preparex(query)
	if err != nil {
		log.Errorf("[170] failed to prepare statment: %v", err)
		return []int64{}, fmt.Errorf("failed to prepare statment to convert usergroup name to ids for default usergroup")
	}
	defer func(stmt1 *sqlx.Stmt) {
		err := stmt1.Close()
		if err != nil {
			log.Errorf("failed to close prepared statement: %v", err)
		}
	}(stmt1)

	rows, err := stmt1.Queryx(args...)
	CheckErr(err, "[176] failed to query user-group names to ids")
	if err != nil {
		return nil, err
	}

	retInt := make([]int64, 0)

	for rows.Next() {
		//iVal, _ := strconv.ParseInt(val, 10, 64)
		var id int64
		err := rows.Scan(&id)
		if err != nil {
			log.Errorf("[185] failed to scan value after query: %v", err)
			return nil, err
		}
		retInt = append(retInt, id)
	}
	err = rows.Close()
	CheckErr(err, "[206] Failed to close rows after default group name conversation")

	return retInt, nil

}

func (dbResource *DbResource) PutContext(key string, val interface{}) {
	dbResource.contextLock.Lock()
	defer dbResource.contextLock.Unlock()

	dbResource.contextCache[key] = val
}

func (dbResource *DbResource) GetContext(key string) interface{} {

	dbResource.contextLock.RLock()
	defer dbResource.contextLock.RUnlock()

	return dbResource.contextCache[key]
}

func (dbResource *DbResource) GetAdminReferenceId() map[string]bool {
	var err error
	var cacheValue interface{}
	adminMap := make(map[string]bool)
	if OlricCache != nil {
		cacheValue, err = OlricCache.Get("administrator_reference_id")
		if err == nil && cacheValue != nil {
			return cacheValue.(map[string]bool)
		}
	}
	userRefId := dbResource.GetUserMembersByGroupName("administrators")
	for _, id := range userRefId {
		adminMap[id] = true
	}

	if OlricCache != nil && userRefId != nil {
		err = OlricCache.PutIfEx("administrator_reference_id", adminMap, 60*time.Minute, olric.IfNotFound)
		CheckErr(err, "Failed to cache admin reference ids")
	}
	return adminMap
}

func GetAdminReferenceIdWithTransaction(transaction *sqlx.Tx) map[string]bool {
	var err error
	var cacheValue interface{}
	adminMap := make(map[string]bool)
	if OlricCache != nil {
		cacheValue, err = OlricCache.Get("administrator_reference_id")
		if err == nil && cacheValue != nil {
			return cacheValue.(map[string]bool)
		}
	}
	userRefId := GetUserMembersByGroupNameWithTransaction("administrators", transaction)
	for _, id := range userRefId {
		adminMap[id] = true
	}

	if OlricCache != nil && userRefId != nil {
		err = OlricCache.PutIfEx("administrator_reference_id", adminMap, 60*time.Minute, olric.IfNotFound)
		//CheckErr(err, "[257] Failed to cache admin reference ids")
	}
	return adminMap
}

func (dbResource *DbResource) IsAdmin(userReferenceId string) bool {
	start := time.Now()
	key := "admin." + userReferenceId
	if OlricCache != nil {
		value, err := OlricCache.Get(key)
		if err == nil && value != nil {
			if value.(bool) == true {
				duration := time.Since(start)
				log.Tracef("[TIMING]IsAdmin Cached[true]: %v", duration)
				return true
			} else {
				duration := time.Since(start)
				log.Tracef("[TIMING] IsAdmin Cached[false]: %v", duration)
				return false
			}
		}
	}
	admins := dbResource.GetAdminReferenceId()
	_, ok := admins[userReferenceId]
	if ok {
		if OlricCache != nil {
			err := OlricCache.PutIfEx(key, true, 5*time.Minute, olric.IfNotFound)
			CheckErr(err, "[285] Failed to set admin id value in olric cache")
		}
		duration := time.Since(start)
		log.Tracef("[TIMING] IsAdmin NotCached[true]: %v", duration)
		return true
	}
	err := OlricCache.PutIfEx(key, false, 5*time.Minute, olric.IfNotFound)
	CheckErr(err, "[291] Failed to set admin id value in olric cache")

	duration := time.Since(start)
	log.Tracef("[TIMING] IsAdmin NotCached[true]: %v", duration)
	return false

}
func IsAdminWithTransaction(userReferenceId string, transaction *sqlx.Tx) bool {
	key := "admin." + userReferenceId
	if OlricCache != nil {
		value, err := OlricCache.Get(key)
		if err == nil && value != nil {
			if value.(bool) == true {
				return true
			} else {
				return false
			}
		}
	}
	admins := GetAdminReferenceIdWithTransaction(transaction)
	_, ok := admins[userReferenceId]
	if ok {
		if OlricCache != nil {
			OlricCache.PutIfEx(key, true, 5*time.Minute, olric.IfNotFound)
			//CheckErr(err, "[320] Failed to set admin id value in olric cache")
		}
		return true
	}
	if OlricCache != nil {
		OlricCache.PutIfEx(key, false, 5*time.Minute, olric.IfNotFound)
	}
	//CheckErr(err, "[327] Failed to set admin id value in olric cache")
	return false

}

func (dbResource *DbResource) TableInfo() *TableInfo {
	return dbResource.tableInfo
}

func (dbResource *DbResource) GetAdminEmailId() string {
	cacheVal := dbResource.GetContext("administrator_email_id")
	if cacheVal == nil {
		userRefId := dbResource.GetUserEmailIdByUsergroupId(2)
		dbResource.PutContext("administrator_email_id", userRefId)
		return userRefId
	} else {
		return cacheVal.(string)
	}
}

func (dbResource *DbResource) GetMailBoxMailsByOffset(mailBoxId int64, start uint32, stop uint32) ([]map[string]interface{}, error) {

	q := statementbuilder.Squirrel.Select("*").From("mail").Where(goqu.Ex{
		"mail_box_id": mailBoxId,
		"deleted":     false,
	}).Offset(uint(start - 1))

	if stop > 0 {
		q = q.Limit(uint(stop - start + 1))
	}

	query, args, err := q.ToSQL()

	if err != nil {
		return nil, err
	}

	stmt1, err := dbResource.Connection.Preparex(query)
	if err != nil {
		log.Errorf("[275] failed to prepare statment: %v", err)
	}
	defer func(stmt1 *sqlx.Stmt) {
		err := stmt1.Close()
		if err != nil {
			log.Errorf("failed to close prepared statement: %v", err)
		}
	}(stmt1)

	row, err := stmt1.Queryx(args...)

	if err != nil {
		return nil, err
	}

	m, _, err := dbResource.ResultToArrayOfMap(row, dbResource.Cruds["mail"].model.GetColumnMap(), nil)
	row.Close()

	return m, err

}

func (dbResource *DbResource) GetMailBoxMailsByUidSequence(mailBoxId int64, start uint32, stop uint32) ([]map[string]interface{}, error) {

	q := statementbuilder.Squirrel.Select("*").From("mail").Where(goqu.Ex{
		"mail_box_id": mailBoxId,
		"deleted":     false,
	}).Where(goqu.Ex{
		"id": goqu.Op{"gte": start},
	})

	if stop > 0 {
		q = q.Where(goqu.Ex{
			"id": goqu.Op{"lte": stop},
		})
	}

	q = q.Order(goqu.C("id").Asc())

	query, args, err := q.ToSQL()

	if err != nil {
		return nil, err
	}

	stmt1, err := dbResource.Connection.Preparex(query)
	if err != nil {
		log.Errorf("[322] failed to prepare statment: %v", err)
	}
	defer func(stmt1 *sqlx.Stmt) {
		err := stmt1.Close()
		if err != nil {
			log.Errorf("failed to close prepared statement: %v", err)
		}
	}(stmt1)

	row, err := stmt1.Queryx(args...)

	if err != nil {
		return nil, err
	}

	m, _, err := dbResource.ResultToArrayOfMap(row, dbResource.Cruds["mail"].model.GetColumnMap(), nil)
	row.Close()

	return m, err

}

func (dbResource *DbResource) GetMailBoxStatus(mailAccountId int64, mailBoxId int64) (*imap.MailboxStatus, error) {

	var unseenCount uint32
	var recentCount uint32
	var uidValidity uint32
	var uidNext uint32
	var messgeCount uint32

	q4, v4, e4 := statementbuilder.Squirrel.Select(goqu.L("count(*)")).From("mail").Where(goqu.Ex{
		"mail_box_id": mailBoxId,
	}).ToSQL()

	if e4 != nil {
		return nil, e4
	}

	stmt1, err := dbResource.Connection.Preparex(q4)
	if err != nil {
		log.Errorf("[362] failed to prepare statment: %v", err)
	}

	r4 := stmt1.QueryRowx(v4...)
	r4.Scan(&messgeCount)
	err = stmt1.Close()
	if err != nil {
		log.Errorf("failed to close prepared statement: %v", err)
	}

	q1, v1, e1 := statementbuilder.Squirrel.Select(goqu.L("count(*)")).From("mail").Where(goqu.Ex{
		"mail_box_id": mailBoxId,
		"seen":        false,
	}).ToSQL()

	if e1 != nil {
		return nil, e1
	}

	stmt1, err = dbResource.Connection.Preparex(q1)
	if err != nil {
		log.Errorf("[384] failed to prepare statment: %v", err)
	}

	r := stmt1.QueryRowx(v1...)
	r.Scan(&unseenCount)
	err = stmt1.Close()
	if err != nil {
		log.Errorf("failed to close prepared statement: %v", err)
	}

	q2, v2, e2 := statementbuilder.Squirrel.Select(goqu.L("count(*)")).From("mail").Where(goqu.Ex{
		"mail_box_id": mailBoxId,
		"recent":      true,
	}).ToSQL()

	if e2 != nil {
		return nil, e2
	}

	stmt1, err = dbResource.Connection.Preparex(q2)
	if err != nil {
		log.Errorf("[405] failed to prepare statment: %v", err)
	}

	r2 := stmt1.QueryRowx(v2...)
	r2.Scan(&recentCount)
	err = stmt1.Close()
	if err != nil {
		log.Errorf("failed to close prepared statement: %v", err)
	}

	q3, v3, e3 := statementbuilder.Squirrel.Select("uidvalidity").From("mail_box").Where(goqu.Ex{
		"id": mailBoxId,
	}).ToSQL()

	if e3 != nil {
		return nil, e3
	}

	stmt1, err = dbResource.Connection.Preparex(q3)
	if err != nil {
		log.Errorf("[425] failed to prepare statment: %v", err)
	}

	r3 := stmt1.QueryRowx(v3...)
	r3.Scan(&uidValidity)
	err = stmt1.Close()
	if err != nil {
		log.Errorf("failed to close prepared statement: %v", err)
	}

	uidNext, _ = dbResource.GetMailboxNextUid(mailBoxId)

	st := imap.NewMailboxStatus("", []imap.StatusItem{imap.StatusUnseen, imap.StatusMessages, imap.StatusRecent, imap.StatusUidNext, imap.StatusUidValidity})

	err = st.Parse([]interface{}{
		string(imap.StatusMessages), messgeCount,
		string(imap.StatusUnseen), unseenCount,
		string(imap.StatusRecent), recentCount,
		string(imap.StatusUidValidity), uidValidity,
		string(imap.StatusUidNext), uidNext,
	})

	return st, err
}

func (dbResource *DbResource) GetFirstUnseenMailSequence(mailBoxId int64) uint32 {

	query, args, err := statementbuilder.Squirrel.Select(goqu.L("min(id)")).From("mail").Where(
		goqu.Ex{
			"mail_box_id": mailBoxId,
			"seen":        false,
		}).ToSQL()

	if err != nil {
		return 0
	}

	var id uint32
	stmt1, err := dbResource.Connection.Preparex(query)
	if err != nil {
		log.Errorf("[465] failed to prepare statment: %v", err)
	}
	defer func(stmt1 *sqlx.Stmt) {
		err := stmt1.Close()
		if err != nil {
			log.Errorf("failed to close prepared statement: %v", err)
		}
	}(stmt1)

	row := stmt1.QueryRowx(args...)
	if row.Err() != nil {
		return 0
	}
	row.Scan(&id)
	return id

}
func (dbResource *DbResource) UpdateMailFlags(mailBoxId int64, mailId int64, newFlags []string) error {

	//log.Printf("Update mail flags for [%v][%v]: %v", mailBoxId, mailId, newFlags)
	seen := false
	recent := false
	deleted := false

	if HasAnyFlag(newFlags, []string{imap.RecentFlag}) {
		recent = true
	} else {
		seen = true
	}

	if HasAnyFlag(newFlags, []string{"\\seen"}) {
		seen = true
		newFlags = backendutil.UpdateFlags(newFlags, imap.RemoveFlags, []string{imap.RecentFlag})
		log.Printf("New flags: [%v]", newFlags)
	}

	if HasAnyFlag(newFlags, []string{"\\expunge", "\\deleted"}) {
		newFlags = backendutil.UpdateFlags(newFlags, imap.RemoveFlags, []string{imap.RecentFlag})
		newFlags = backendutil.UpdateFlags(newFlags, imap.AddFlags, []string{"\\Seen"})
		log.Printf("New flags: [%v]", newFlags)
		deleted = true
		seen = true
	}

	query, args, err := statementbuilder.Squirrel.
		Update("mail").
		Set(goqu.Record{
			"flags":   strings.Join(newFlags, ","),
			"seen":    seen,
			"recent":  recent,
			"deleted": deleted,
		}).
		Where(goqu.Ex{
			"mail_box_id": mailBoxId,
			"id":          mailId,
		}).ToSQL()
	if err != nil {
		return err
	}

	_, err = dbResource.db.Exec(query, args...)
	return err

}
func (dbResource *DbResource) ExpungeMailBox(mailBoxId int64) (int64, error) {

	selectQuery, args, err := statementbuilder.Squirrel.Select("id").From("mail").Where(
		goqu.Ex{
			"mail_box_id": mailBoxId,
			"deleted":     true,
		},
	).ToSQL()

	if err != nil {
		return 0, err
	}

	stmt1, err := dbResource.Connection.Preparex(selectQuery)
	if err != nil {
		log.Errorf("[544] failed to prepare statment: %v", err)
	}
	defer func(stmt1 *sqlx.Stmt) {
		err := stmt1.Close()
		if err != nil {
			log.Errorf("failed to close prepared statement: %v", err)
		}
	}(stmt1)

	rows, err := stmt1.Queryx(args...)
	if err != nil {
		return 0, err
	}

	ids := make([]interface{}, 0)

	for rows.Next() {
		var id int64
		rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()

	if len(ids) < 1 {
		return 0, nil
	}

	query, args, err := statementbuilder.Squirrel.Delete("mail_mail_id_has_usergroup_usergroup_id").Where(goqu.Ex{
		"mail_id": ids,
	}).ToSQL()

	if err != nil {
		log.Printf("Query: %v", query)
		return 0, err
	}

	_, err = dbResource.db.Exec(query, args...)
	if err != nil {
		return 0, err
	}

	query, args, err = statementbuilder.Squirrel.Delete("mail").Where(goqu.Ex{
		"id": ids,
	}).ToSQL()
	if err != nil {
		return 0, err
	}

	result, err := dbResource.db.Exec(query, args...)
	if err != nil {
		log.Printf("Query: %v", query)
		return 0, err
	}

	return result.RowsAffected()

}

func (dbResource *DbResource) GetMailboxNextUid(mailBoxId int64) (uint32, error) {

	var uidNext int64
	q5, v5, e5 := statementbuilder.Squirrel.Select("max(id)").From("mail").Where(goqu.Ex{
		"mail_box_id": mailBoxId,
	}).ToSQL()

	if e5 != nil {
		return 1, e5
	}

	stmt1, err := dbResource.Connection.Preparex(q5)
	if err != nil {
		log.Errorf("[615] failed to prepare statment: %v", err)
		return 0, err
	}
	defer func(stmt1 *sqlx.Stmt) {
		err := stmt1.Close()
		if err != nil {
			log.Errorf("failed to close prepared statement: %v", err)
		}
	}(stmt1)

	r5 := stmt1.QueryRowx(v5...)
	err = r5.Scan(&uidNext)
	return uint32(int32(uidNext) + 1), err

}
