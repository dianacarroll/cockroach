// Copyright 2016 The Cockroach Authors.
//
// Licensed as a CockroachDB Enterprise file under the Cockroach Community
// License (the "License"); you may not use this file except in compliance with
// the License. You may obtain a copy of the License at
//
//     https://github.com/cockroachdb/cockroach/blob/master/licenses/CCL.txt

package backupccl

import (
	"context"
	"sort"

	"github.com/cockroachdb/cockroach/pkg/ccl/storageccl"
	"github.com/cockroachdb/cockroach/pkg/jobs/jobspb"
	"github.com/cockroachdb/cockroach/pkg/keys"
	"github.com/cockroachdb/cockroach/pkg/kv"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/sql"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/catalogkv"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/descpb"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/sessiondata"
	"github.com/cockroachdb/cockroach/pkg/sql/sqlbase"
	"github.com/cockroachdb/cockroach/pkg/util/hlc"
	"github.com/cockroachdb/errors"
)

type descriptorsMatched struct {
	// All descriptors that match targets plus their parent databases.
	descs []sqlbase.Descriptor

	// The databases from which all tables were matched (eg a.* or DATABASE a).
	expandedDB []descpb.ID

	// Explicitly requested DBs (e.g. DATABASE a).
	requestedDBs []sqlbase.DatabaseDescriptor
}

func (d descriptorsMatched) checkExpansions(coveredDBs []descpb.ID) error {
	covered := make(map[descpb.ID]bool)
	for _, i := range coveredDBs {
		covered[i] = true
	}
	for _, i := range d.requestedDBs {
		if !covered[i.GetID()] {
			return errors.Errorf("cannot RESTORE DATABASE from a backup of individual tables (use SHOW BACKUP to determine available tables)")
		}
	}
	for _, i := range d.expandedDB {
		if !covered[i] {
			return errors.Errorf("cannot RESTORE <database>.* from a backup of individual tables (use SHOW BACKUP to determine available tables)")
		}
	}
	return nil
}

// descriptorResolver is the helper struct that enables reuse of the
// standard name resolution algorithm.
type descriptorResolver struct {
	descByID map[descpb.ID]sqlbase.Descriptor
	// Map: db name -> dbID
	dbsByName map[string]descpb.ID
	// Map: dbID -> obj name -> obj ID
	objsByName map[descpb.ID]map[string]descpb.ID
}

// LookupSchema implements the tree.ObjectNameTargetResolver interface.
// TODO(ajwerner): Update this for user-defined schemas.
func (r *descriptorResolver) LookupSchema(
	_ context.Context, dbName, scName string,
) (bool, tree.SchemaMeta, error) {
	if scName != tree.PublicSchema {
		return false, nil, nil
	}
	if dbID, ok := r.dbsByName[dbName]; ok {
		if dbDesc, ok := r.descByID[dbID].(sqlbase.DatabaseDescriptor); ok {
			return true, dbDesc, nil
		}
	}
	return false, nil, nil
}

// LookupObject implements the tree.ObjectNameExistingResolver interface.
func (r *descriptorResolver) LookupObject(
	_ context.Context, flags tree.ObjectLookupFlags, dbName, scName, obName string,
) (bool, tree.NameResolutionResult, error) {
	if flags.RequireMutable {
		panic("did not expect request for mutable descriptor")
	}
	if scName != tree.PublicSchema {
		return false, nil, nil
	}
	dbID, ok := r.dbsByName[dbName]
	if !ok {
		return false, nil, nil
	}
	if objMap, ok := r.objsByName[dbID]; ok {
		if objID, ok := objMap[obName]; ok {
			return true, r.descByID[objID], nil
		}
	}
	return false, nil, nil
}

// newDescriptorResolver prepares a descriptorResolver for the given
// known set of descriptors.
func newDescriptorResolver(descs []sqlbase.Descriptor) (*descriptorResolver, error) {
	r := &descriptorResolver{
		descByID:   make(map[descpb.ID]sqlbase.Descriptor),
		dbsByName:  make(map[string]descpb.ID),
		objsByName: make(map[descpb.ID]map[string]descpb.ID),
	}

	// Iterate to find the databases first. We need that because we also
	// check the ParentID for tables, and all the valid parents must be
	// known before we start to check that.
	for _, desc := range descs {
		if _, isDB := desc.(sqlbase.DatabaseDescriptor); isDB {
			if _, ok := r.dbsByName[desc.GetName()]; ok {
				return nil, errors.Errorf("duplicate database name: %q used for ID %d and %d",
					desc.GetName(), r.dbsByName[desc.GetName()], desc.GetID())
			}
			r.dbsByName[desc.GetName()] = desc.GetID()
		}

		// Incidentally, also remember all the descriptors by ID.
		if prevDesc, ok := r.descByID[desc.GetID()]; ok {
			return nil, errors.Errorf("duplicate descriptor ID: %d used by %q and %q",
				desc.GetID(), prevDesc.GetName(), desc.GetName())
		}
		r.descByID[desc.GetID()] = desc
	}

	// registerDesc is a closure that registers a Descriptor into the resolver's
	// object registry.
	registerDesc := func(parentID descpb.ID, desc sqlbase.Descriptor, kind string) error {
		parentDesc, ok := r.descByID[parentID]
		if !ok {
			return errors.Errorf("%s %q has unknown ParentID %d", kind, desc.GetName(), parentID)
		}
		if _, ok := r.dbsByName[parentDesc.GetName()]; !ok {
			return errors.Errorf("%s %q's ParentID %d (%q) is not a database",
				kind, desc.GetName(), parentID, parentDesc.GetName())
		}
		objMap := r.objsByName[parentDesc.GetID()]
		if objMap == nil {
			objMap = make(map[string]descpb.ID)
		}
		if _, ok := objMap[desc.GetName()]; ok {
			return errors.Errorf("duplicate %s name: %q.%q used for ID %d and %d",
				kind, parentDesc.GetName(), desc.GetName(), desc.GetID(), objMap[desc.GetName()])
		}
		objMap[desc.GetName()] = desc.GetID()
		r.objsByName[parentDesc.GetID()] = objMap
		return nil
	}

	// Now on to the tables and types.
	for _, desc := range descs {
		if desc.Dropped() {
			continue
		}
		var typeToRegister string
		switch desc.(type) {
		case sqlbase.TableDescriptor:
			typeToRegister = "table"
		case sqlbase.TypeDescriptor:
			typeToRegister = "type"
		}
		if typeToRegister != "" {
			if err := registerDesc(desc.GetParentID(), desc, typeToRegister); err != nil {
				return nil, err
			}
		}
	}

	return r, nil
}

// descriptorsMatchingTargets returns the descriptors that match the targets. A
// database descriptor is included in this set if it matches the targets (or the
// session database) or if one of its tables matches the targets. All expanded
// DBs, via either `foo.*` or `DATABASE foo` are noted, as are those explicitly
// named as DBs (e.g. with `DATABASE foo`, not `foo.*`). These distinctions are
// used e.g. by RESTORE.
//
// This is guaranteed to not return duplicates.
func descriptorsMatchingTargets(
	ctx context.Context,
	currentDatabase string,
	searchPath sessiondata.SearchPath,
	descriptors []sqlbase.Descriptor,
	targets tree.TargetList,
) (descriptorsMatched, error) {
	// TODO(dan): once CockroachDB supports schemas in addition to
	// catalogs, then this method will need to support it.

	ret := descriptorsMatched{}

	resolver, err := newDescriptorResolver(descriptors)
	if err != nil {
		return ret, err
	}

	alreadyRequestedDBs := make(map[descpb.ID]struct{})
	alreadyExpandedDBs := make(map[descpb.ID]struct{})
	// Process all the DATABASE requests.
	for _, d := range targets.Databases {
		dbID, ok := resolver.dbsByName[string(d)]
		if !ok {
			return ret, errors.Errorf("unknown database %q", d)
		}
		if _, ok := alreadyRequestedDBs[dbID]; !ok {
			desc := resolver.descByID[dbID]
			ret.descs = append(ret.descs, desc)
			ret.requestedDBs = append(ret.requestedDBs,
				desc.(sqlbase.DatabaseDescriptor))
			ret.expandedDB = append(ret.expandedDB, dbID)
			alreadyRequestedDBs[dbID] = struct{}{}
			alreadyExpandedDBs[dbID] = struct{}{}
		}
	}

	alreadyRequestedTypes := make(map[descpb.ID]struct{})
	maybeAddTypeDesc := func(id descpb.ID) {
		if _, ok := alreadyRequestedTypes[id]; !ok {
			// Cross database type references have been disabled, so we don't
			// need to request the parent database because it has already been
			// requested by the table that holds this type.
			alreadyRequestedTypes[id] = struct{}{}
			ret.descs = append(ret.descs, resolver.descByID[id])
		}
	}
	getTypeByID := func(id descpb.ID) (sqlbase.TypeDescriptor, error) {
		desc, ok := resolver.descByID[id]
		if !ok {
			return nil, errors.Newf("type with ID %d not found", id)
		}
		typeDesc, ok := desc.(sqlbase.TypeDescriptor)
		if !ok {
			return nil, errors.Newf("descriptor %d is not a type, but a %T", id, desc)
		}
		return typeDesc, nil
	}

	// Process all the TABLE requests.
	// Pulling in a table needs to pull in the underlying database too.
	alreadyRequestedTables := make(map[descpb.ID]struct{})
	for _, pattern := range targets.Tables {
		var err error
		pattern, err = pattern.NormalizeTablePattern()
		if err != nil {
			return ret, err
		}

		switch p := pattern.(type) {
		case *tree.TableName:
			// TODO: As part of work for #34240, this should not be a TableName.
			//  Instead, it should be an UnresolvedObjectName.
			un := p.ToUnresolvedObjectName()
			found, prefix, descI, err := tree.ResolveExisting(ctx, un, resolver, tree.ObjectLookupFlags{}, currentDatabase, searchPath)
			if err != nil {
				return ret, err
			}
			p.ObjectNamePrefix = prefix
			doesNotExistErr := errors.Errorf(`table %q does not exist`, tree.ErrString(p))
			if !found {
				return ret, doesNotExistErr
			}
			tableDesc := descI.(sqlbase.TableDescriptor)
			// If tableDesc is nil, then we resolved a type instead, so error out.
			if tableDesc == nil {
				return ret, doesNotExistErr
			}

			// Verify that the table is in the correct state.
			if err := catalog.FilterDescriptorState(tableDesc); err != nil {
				// Return a does not exist error if explicitly asking for this table.
				return ret, doesNotExistErr
			}

			// If the parent database is not requested already, request it now.
			parentID := tableDesc.GetParentID()
			if _, ok := alreadyRequestedDBs[parentID]; !ok {
				parentDesc := resolver.descByID[parentID]
				ret.descs = append(ret.descs, parentDesc)
				alreadyRequestedDBs[parentID] = struct{}{}
			}
			// Then request the table itself.
			if _, ok := alreadyRequestedTables[tableDesc.GetID()]; !ok {
				alreadyRequestedTables[tableDesc.GetID()] = struct{}{}
				ret.descs = append(ret.descs, tableDesc)
			}
			// Get all the types used by this table.
			typeIDs, err := tableDesc.GetAllReferencedTypeIDs(getTypeByID)
			if err != nil {
				return ret, err
			}
			for _, id := range typeIDs {
				maybeAddTypeDesc(id)
			}

		case *tree.AllTablesSelector:
			found, descI, err := p.ObjectNamePrefix.Resolve(ctx, resolver, currentDatabase, searchPath)
			if err != nil {
				return ret, err
			}
			if !found {
				return ret, sqlbase.NewInvalidWildcardError(tree.ErrString(p))
			}
			desc := descI.(sqlbase.DatabaseDescriptor)

			// If the database is not requested already, request it now.
			dbID := desc.GetID()
			if _, ok := alreadyRequestedDBs[dbID]; !ok {
				ret.descs = append(ret.descs, desc)
				alreadyRequestedDBs[dbID] = struct{}{}
			}

			// Then request the expansion.
			if _, ok := alreadyExpandedDBs[desc.GetID()]; !ok {
				ret.expandedDB = append(ret.expandedDB, desc.GetID())
				alreadyExpandedDBs[desc.GetID()] = struct{}{}
			}

		default:
			return ret, errors.Errorf("unknown pattern %T: %+v", pattern, pattern)
		}
	}

	// Then process the database expansions.
	for dbID := range alreadyExpandedDBs {
		for _, id := range resolver.objsByName[dbID] {
			desc := resolver.descByID[id]
			switch desc := desc.(type) {
			case sqlbase.TableDescriptor:
				if err := catalog.FilterDescriptorState(desc); err != nil {
					// Don't include this table in the expansion since it's not in a valid
					// state. Silently fail since this table was not directly requested,
					// but was just part of an expansion.
					continue
				}
				if _, ok := alreadyRequestedTables[id]; !ok {
					ret.descs = append(ret.descs, desc)
				}
				// Get all the types used by this table.
				typeIDs, err := desc.GetAllReferencedTypeIDs(getTypeByID)
				if err != nil {
					return ret, err
				}
				for _, id := range typeIDs {
					maybeAddTypeDesc(id)
				}
			case sqlbase.TypeDescriptor:
				maybeAddTypeDesc(desc.GetID())
			}
		}
	}

	return ret, nil
}

// getRelevantDescChanges finds the changes between start and end time to the
// SQL descriptors matching `descs` or `expandedDBs`, ordered by time. A
// descriptor revision matches if it is an earlier revision of a descriptor in
// descs (same ID) or has parentID in `expanded`. Deleted descriptors are
// represented as nil. Fills in the `priorIDs` map in the process, which maps
// a descriptor the the ID by which it was previously known (e.g pre-TRUNCATE).
func getRelevantDescChanges(
	ctx context.Context,
	db *kv.DB,
	startTime, endTime hlc.Timestamp,
	descs []sqlbase.Descriptor,
	expanded []descpb.ID,
	priorIDs map[descpb.ID]descpb.ID,
) ([]BackupManifest_DescriptorRevision, error) {

	allChanges, err := getAllDescChanges(ctx, db, startTime, endTime, priorIDs)
	if err != nil {
		return nil, err
	}

	// If no descriptors changed, we can just stop now and have RESTORE use the
	// normal list of descs (i.e. as of endTime).
	if len(allChanges) == 0 {
		return nil, nil
	}

	// interestingChanges will be every descriptor change relevant to the backup.
	var interestingChanges []BackupManifest_DescriptorRevision

	// interestingIDs are the descriptor for which we're interested in capturing
	// changes. This is initially the descriptors matched (as of endTime) by our
	// target spec, plus those that belonged to a DB that our spec expanded at any
	// point in the interval.
	interestingIDs := make(map[descpb.ID]struct{}, len(descs))

	// The descriptors that currently (endTime) match the target spec (desc) are
	// obviously interesting to our backup.
	for _, i := range descs {
		interestingIDs[i.GetID()] = struct{}{}
		if table, isTable := i.(sqlbase.TableDescriptor); isTable {

			for j := table.GetReplacementOf().ID; j != descpb.InvalidID; j = priorIDs[j] {
				interestingIDs[j] = struct{}{}
			}
		}
		// TODO (rohany): Once we start tracking modification time on type
		//  descriptors we need to consider them here.
	}

	// We're also interested in any desc that belonged to a DB we're backing up.
	// We'll start by looking at all descriptors as of the beginning of the
	// interval and add to the set of IDs that we are interested any descriptor that
	// belongs to one of the parents we care about.
	interestingParents := make(map[descpb.ID]struct{}, len(expanded))
	for _, i := range expanded {
		interestingParents[i] = struct{}{}
	}

	if !startTime.IsEmpty() {
		starting, err := loadAllDescs(ctx, db, startTime)
		if err != nil {
			return nil, err
		}
		for _, i := range starting {
			// TODO(ajwerner): Determine whether types and schemas need any special
			// treatment here.
			if table, isTable := i.(sqlbase.TableDescriptor); isTable {
				// We need to add to interestingIDs so that if we later see a delete for
				// this ID we still know it is interesting to us, even though we will not
				// have a parentID at that point (since the delete is a nil desc).
				if _, ok := interestingParents[table.GetParentID()]; ok {
					interestingIDs[table.GetID()] = struct{}{}
				}
			}
			if _, ok := interestingIDs[i.GetID()]; ok {
				desc := i
				// We inject a fake "revision" that captures the starting state for
				// matched descriptor, to allow restoring to times before its first rev
				// actually inside the window. This likely ends up duplicating the last
				// version in the previous BACKUP descriptor, but avoids adding more
				// complicated special-cases in RESTORE, so it only needs to look in a
				// single BACKUP to restore to a particular time.
				initial := BackupManifest_DescriptorRevision{Time: startTime, ID: i.GetID(), Desc: desc.DescriptorProto()}
				interestingChanges = append(interestingChanges, initial)
			}
		}
	}

	for _, change := range allChanges {
		// A change to an ID that we are interested in is obviously interesting --
		// a change is also interesting if it is to a table that has a parent that
		// we are interested and thereafter it also becomes an ID in which we are
		// interested in changes (since, as mentioned above, to decide if deletes
		// are interesting).
		if _, ok := interestingIDs[change.ID]; ok {
			interestingChanges = append(interestingChanges, change)
		} else if change.Desc != nil {
			if table := sqlbase.TableFromDescriptor(change.Desc, hlc.Timestamp{}); table != nil {
				if _, ok := interestingParents[table.ParentID]; ok {
					interestingIDs[table.ID] = struct{}{}
					interestingChanges = append(interestingChanges, change)
				}
			}
		}
	}

	sort.Slice(interestingChanges, func(i, j int) bool {
		return interestingChanges[i].Time.Less(interestingChanges[j].Time)
	})

	return interestingChanges, nil
}

// getAllDescChanges gets every sql descriptor change between start and end time
// returning its ID, content and the change time (with deletions represented as
// nil content).
func getAllDescChanges(
	ctx context.Context,
	db *kv.DB,
	startTime, endTime hlc.Timestamp,
	priorIDs map[descpb.ID]descpb.ID,
) ([]BackupManifest_DescriptorRevision, error) {
	startKey := keys.TODOSQLCodec.TablePrefix(keys.DescriptorTableID)
	endKey := startKey.PrefixEnd()

	allRevs, err := storageccl.GetAllRevisions(ctx, db, startKey, endKey, startTime, endTime)
	if err != nil {
		return nil, err
	}

	var res []BackupManifest_DescriptorRevision

	for _, revs := range allRevs {
		id, err := keys.TODOSQLCodec.DecodeDescMetadataID(revs.Key)
		if err != nil {
			return nil, err
		}
		for _, rev := range revs.Values {
			r := BackupManifest_DescriptorRevision{ID: descpb.ID(id), Time: rev.Timestamp}
			if len(rev.RawBytes) != 0 {
				var desc descpb.Descriptor
				if err := rev.GetProto(&desc); err != nil {
					return nil, err
				}
				r.Desc = &desc
				t := sqlbase.TableFromDescriptor(&desc, rev.Timestamp)
				if t != nil && t.ReplacementOf.ID != descpb.InvalidID {
					priorIDs[t.ID] = t.ReplacementOf.ID
				}
				// TODO (rohany): Once we track modification time on type descriptors,
				//  they need to be checked for updates here.
			}
			res = append(res, r)
		}
	}
	return res, nil
}

func ensureInterleavesIncluded(tables []sqlbase.TableDescriptor) error {
	inBackup := make(map[descpb.ID]bool, len(tables))
	for _, t := range tables {
		inBackup[t.GetID()] = true
	}

	for _, table := range tables {
		if err := table.ForeachNonDropIndex(func(index *descpb.IndexDescriptor) error {
			for _, a := range index.Interleave.Ancestors {
				if !inBackup[a.TableID] {
					return errors.Errorf(
						"cannot backup table %q without interleave parent (ID %d)", table.GetName(), a.TableID,
					)
				}
			}
			for _, c := range index.InterleavedBy {
				if !inBackup[c.Table] {
					return errors.Errorf(
						"cannot backup table %q without interleave child table (ID %d)", table.GetName(), c.Table,
					)
				}
			}
			return nil
		}); err != nil {
			return err
		}
	}
	return nil
}

func loadAllDescs(
	ctx context.Context, db *kv.DB, asOf hlc.Timestamp,
) ([]sqlbase.Descriptor, error) {
	var allDescs []sqlbase.Descriptor
	if err := db.Txn(
		ctx,
		func(ctx context.Context, txn *kv.Txn) (err error) {
			txn.SetFixedTimestamp(ctx, asOf)
			allDescs, err = catalogkv.GetAllDescriptors(ctx, txn, keys.SystemSQLCodec)
			return err
		}); err != nil {
		return nil, err
	}
	return allDescs, nil
}

// ResolveTargetsToDescriptors performs name resolution on a set of targets and
// returns the resulting descriptors.
func ResolveTargetsToDescriptors(
	ctx context.Context, p sql.PlanHookState, endTime hlc.Timestamp, targets *tree.TargetList,
) ([]sqlbase.Descriptor, []descpb.ID, error) {
	allDescs, err := loadAllDescs(ctx, p.ExecCfg().DB, endTime)
	if err != nil {
		return nil, nil, err
	}

	if targets == nil {
		return fullClusterTargetsBackup(allDescs)
	}

	var matched descriptorsMatched
	if matched, err = descriptorsMatchingTargets(ctx,
		p.CurrentDatabase(), p.CurrentSearchPath(), allDescs, *targets); err != nil {
		return nil, nil, err
	}

	// Ensure interleaved tables appear after their parent. Since parents must be
	// created before their children, simply sorting by ID accomplishes this.
	sort.Slice(matched.descs, func(i, j int) bool { return matched.descs[i].GetID() < matched.descs[j].GetID() })
	return matched.descs, matched.expandedDB, nil
}

// fullClusterTargetsBackup returns the same descriptors referenced in
// fullClusterTargets, but rather than returning the entire database
// descriptor as the second argument, it only returns their IDs.
func fullClusterTargetsBackup(
	allDescs []sqlbase.Descriptor,
) ([]sqlbase.Descriptor, []descpb.ID, error) {
	fullClusterDescs, fullClusterDBs, err := fullClusterTargets(allDescs)
	if err != nil {
		return nil, nil, err
	}

	fullClusterDBIDs := make([]descpb.ID, 0)
	for _, desc := range fullClusterDBs {
		fullClusterDBIDs = append(fullClusterDBIDs, desc.GetID())
	}
	return fullClusterDescs, fullClusterDBIDs, nil
}

// fullClusterTargets returns all of the tableDescriptors to be included in a
// full cluster backup, and all the user databases.
func fullClusterTargets(
	allDescs []sqlbase.Descriptor,
) ([]sqlbase.Descriptor, []*sqlbase.ImmutableDatabaseDescriptor, error) {
	fullClusterDescs := make([]sqlbase.Descriptor, 0, len(allDescs))
	fullClusterDBs := make([]*sqlbase.ImmutableDatabaseDescriptor, 0)

	systemTablesToBackup := make(map[string]struct{}, len(fullClusterSystemTables))
	for _, tableName := range fullClusterSystemTables {
		systemTablesToBackup[tableName] = struct{}{}
	}

	for _, desc := range allDescs {
		switch desc := desc.(type) {
		case sqlbase.DatabaseDescriptor:
			dbDesc := sqlbase.NewImmutableDatabaseDescriptor(*desc.DatabaseDesc())
			fullClusterDescs = append(fullClusterDescs, desc)
			if dbDesc.GetID() != sqlbase.SystemDB.GetID() {
				// The only database that isn't being fully backed up is the system DB.
				fullClusterDBs = append(fullClusterDBs, dbDesc)
			}
		case sqlbase.TableDescriptor:
			if desc.GetParentID() == keys.SystemDatabaseID {
				// Add only the system tables that we plan to include in a full cluster
				// backup.
				if _, ok := systemTablesToBackup[desc.GetName()]; ok {
					fullClusterDescs = append(fullClusterDescs, desc)
				}
			} else {
				// Add all user tables that are not in a DROP state.
				if desc.GetState() != descpb.TableDescriptor_DROP {
					fullClusterDescs = append(fullClusterDescs, desc)
				}
			}
		case sqlbase.TypeDescriptor:
			fullClusterDescs = append(fullClusterDescs, desc)
		}
	}
	return fullClusterDescs, fullClusterDBs, nil
}

func lookupDatabaseID(
	ctx context.Context, txn *kv.Txn, codec keys.SQLCodec, name string,
) (descpb.ID, error) {
	found, id, err := catalogkv.LookupDatabaseID(ctx, txn, codec, name)
	if err != nil {
		return descpb.InvalidID, err
	}
	if !found {
		return descpb.InvalidID, errors.Errorf("could not find ID for database %s", name)
	}
	return id, nil
}

// CheckObjectExists returns an error if an object already exists with a given
// parent, parent schema and name.
func CheckObjectExists(
	ctx context.Context,
	txn *kv.Txn,
	codec keys.SQLCodec,
	parentID descpb.ID,
	parentSchemaID descpb.ID,
	name string,
) error {
	found, id, err := catalogkv.LookupObjectID(ctx, txn, codec, parentID, parentSchemaID, name)
	if err != nil {
		return err
	}
	if found {
		// Find what object we collided with.
		desc, err := catalogkv.GetAnyDescriptorByID(ctx, txn, codec, id, catalogkv.Immutable)
		if err != nil {
			return sqlbase.WrapErrorWhileConstructingObjectAlreadyExistsErr(err)
		}
		return sqlbase.MakeObjectAlreadyExistsError(desc.DescriptorProto(), name)
	}
	return nil
}

func fullClusterTargetsRestore(
	allDescs []sqlbase.Descriptor,
) ([]sqlbase.Descriptor, []sqlbase.DatabaseDescriptor, []jobspb.RestoreDetails_Tenant, error) {
	fullClusterDescs, fullClusterDBs, err := fullClusterTargets(allDescs)
	if err != nil {
		return nil, nil, nil, err
	}
	filteredDescs := make([]sqlbase.Descriptor, 0, len(fullClusterDescs))
	for _, desc := range fullClusterDescs {
		if _, isDefaultDB := sqlbase.DefaultUserDBs[desc.GetName()]; !isDefaultDB && desc.GetID() != keys.SystemDatabaseID {
			filteredDescs = append(filteredDescs, desc)
		}
	}
	filteredDBs := make([]sqlbase.DatabaseDescriptor, 0, len(fullClusterDBs))
	for _, db := range fullClusterDBs {
		if _, isDefaultDB := sqlbase.DefaultUserDBs[db.GetName()]; !isDefaultDB && db.GetID() != keys.SystemDatabaseID {
			filteredDBs = append(filteredDBs, db)
		}
	}

	return filteredDescs, filteredDBs, nil, nil
}

func selectTargets(
	ctx context.Context,
	p sql.PlanHookState,
	backupManifests []BackupManifest,
	targets tree.TargetList,
	descriptorCoverage tree.DescriptorCoverage,
	asOf hlc.Timestamp,
) ([]sqlbase.Descriptor, []sqlbase.DatabaseDescriptor, []jobspb.RestoreDetails_Tenant, error) {
	allDescs, lastBackupManifest := loadSQLDescsFromBackupsAtTime(backupManifests, asOf)

	if descriptorCoverage == tree.AllDescriptors {
		return fullClusterTargetsRestore(allDescs)
	}

	if targets.Tenant != (roachpb.TenantID{}) {
		for _, tenant := range lastBackupManifest.Tenants {
			// TODO(dt): for now it is zero-or-one but when that changes, we should
			// either keep it sorted or build a set here.
			if tenant.ID == targets.Tenant.ToUint64() {
				return nil, nil, []jobspb.RestoreDetails_Tenant{{ID: tenant.ID, Info: tenant.Info}}, nil
			}
		}
		return nil, nil, nil, errors.Errorf("tenant %d not in backup", targets.Tenant.ToUint64())
	}

	matched, err := descriptorsMatchingTargets(ctx,
		p.CurrentDatabase(), p.CurrentSearchPath(), allDescs, targets)
	if err != nil {
		return nil, nil, nil, err
	}

	if len(matched.descs) == 0 {
		return nil, nil, nil, errors.Errorf("no tables or databases matched the given targets: %s", tree.ErrString(&targets))
	}

	if lastBackupManifest.FormatVersion >= BackupFormatDescriptorTrackingVersion {
		if err := matched.checkExpansions(lastBackupManifest.CompleteDbs); err != nil {
			return nil, nil, nil, err
		}
	}

	return matched.descs, matched.requestedDBs, nil, nil
}
