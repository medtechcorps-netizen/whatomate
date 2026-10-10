package dbcatalog

// Queries use only pg_catalog builtins and public-schema catalog predicates.
// No stored application function, view, or dynamically supplied SQL is run.
type catalogQuery struct{ kind, sql string }

// Whole-index and exclusion deparsing includes index storage options. Those
// and tablespace identities are count-only; bind only resolved semantic fields.
// Reading the optional nulls flag through JSON keeps this query valid on PG14.
const indexSemanticsSQL = `pg_catalog.jsonb_build_array(am.amname,x.indnatts,x.indnkeyatts,x.indisunique,x.indimmediate,
 COALESCE((pg_catalog.to_jsonb(x)->>'indnullsnotdistinct')::boolean,false),
 COALESCE(pg_catalog.pg_get_expr(x.indpred,x.indrelid,false),''),
 (SELECT pg_catalog.jsonb_agg(pg_catalog.jsonb_build_array(k.position,pg_catalog.pg_get_indexdef(x.indexrelid,k.position,false),
   COALESCE(onsp.nspname,''),COALESCE(opc.opcname,''),COALESCE(cnsp.nspname,''),COALESCE(coll.collname,''),
   CASE WHEN k.position<=x.indnkeyatts THEN x.indoption[k.position-1]::integer ELSE 0 END) ORDER BY k.position)
  FROM pg_catalog.generate_series(1,x.indnatts) k(position)
  LEFT JOIN pg_catalog.pg_opclass opc ON k.position<=x.indnkeyatts AND opc.oid=x.indclass[k.position-1]
  LEFT JOIN pg_catalog.pg_namespace onsp ON onsp.oid=opc.opcnamespace
  LEFT JOIN pg_catalog.pg_collation coll ON k.position<=x.indnkeyatts AND coll.oid=x.indcollation[k.position-1]
  LEFT JOIN pg_catalog.pg_namespace cnsp ON cnsp.oid=coll.collnamespace))`

var queries = []catalogQuery{
	{"relations", `SELECT c.relname::text, ''::text, pg_catalog.jsonb_build_object('relkind',c.relkind::text,'rls',c.relrowsecurity,'force_rls',c.relforcerowsecurity,'owner',c.relowner::text,'acl',` + aclExpr("COALESCE(c.relacl,pg_catalog.acldefault('r',c.relowner))") + `,'security_invoker',COALESCE('security_invoker=true'=ANY(c.reloptions),false),'view',CASE WHEN c.relkind IN ('v','m') THEN pg_catalog.pg_get_viewdef(c.oid,false) ELSE '' END,
 'partition_key',CASE WHEN c.relkind='p' THEN pg_catalog.pg_get_partkeydef(c.oid) ELSE '' END,
 'partition_bound',CASE WHEN c.relispartition THEN pg_catalog.pg_get_expr(c.relpartbound,c.oid,false) ELSE '' END,
 'parents',(SELECT COALESCE(pg_catalog.jsonb_agg(pg_catalog.jsonb_build_array(pn.nspname,p.relname) ORDER BY pn.nspname,p.relname),'[]'::jsonb)::text FROM pg_catalog.pg_inherits i JOIN pg_catalog.pg_class p ON p.oid=i.inhparent JOIN pg_catalog.pg_namespace pn ON pn.oid=p.relnamespace WHERE i.inhrelid=c.oid AND pn.nspname='public'))::text FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public' AND c.relkind IN ('r','p','v','m','f')`},
	{"columns", `SELECT a.attname::text,c.relname::text,pg_catalog.jsonb_build_object('position',a.attnum,'type',pg_catalog.format_type(a.atttypid,a.atttypmod),'not_null',a.attnotnull,'default',COALESCE(pg_catalog.pg_get_expr(d.adbin,d.adrelid,false),''),'identity',a.attidentity::text,'generated',a.attgenerated::text,'collation',CASE WHEN a.attcollation=0 THEN '' ELSE COALESCE(cn.nspname||'.'||coll.collname,'') END,'acl',` + aclExpr("COALESCE(a.attacl,'{}'::pg_catalog.aclitem[])") + `)::text FROM pg_catalog.pg_attribute a JOIN pg_catalog.pg_class c ON c.oid=a.attrelid JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace LEFT JOIN pg_catalog.pg_attrdef d ON d.adrelid=c.oid AND d.adnum=a.attnum LEFT JOIN pg_catalog.pg_collation coll ON coll.oid=a.attcollation LEFT JOIN pg_catalog.pg_namespace cn ON cn.oid=coll.collnamespace WHERE n.nspname='public' AND c.relkind IN ('r','p','v','m','f') AND a.attnum>0 AND NOT a.attisdropped`},
	{"constraints", `SELECT co.conname::text,COALESCE(c.relname,t.typname)::text,pg_catalog.jsonb_build_object('type',co.contype::text,'definition',CASE WHEN co.contype='x' THEN pg_catalog.jsonb_build_array(` + indexSemanticsSQL + `,co.connoinherit,
 (SELECT pg_catalog.jsonb_agg(pg_catalog.jsonb_build_array(ons.nspname,op.oprname,pg_catalog.format_type(op.oprleft,NULL),pg_catalog.format_type(op.oprright,NULL)) ORDER BY e.position)
  FROM pg_catalog.unnest(co.conexclop) WITH ORDINALITY e(operator_oid,position)
  JOIN pg_catalog.pg_operator op ON op.oid=e.operator_oid JOIN pg_catalog.pg_namespace ons ON ons.oid=op.oprnamespace))::text
 ELSE pg_catalog.pg_get_constraintdef(co.oid,false) END,'validated',co.convalidated,'deferrable',co.condeferrable,'deferred',co.condeferred)::text FROM pg_catalog.pg_constraint co JOIN pg_catalog.pg_namespace n ON n.oid=co.connamespace LEFT JOIN pg_catalog.pg_class c ON c.oid=co.conrelid LEFT JOIN pg_catalog.pg_type t ON t.oid=co.contypid LEFT JOIN pg_catalog.pg_index x ON x.indexrelid=co.conindid LEFT JOIN pg_catalog.pg_class i ON i.oid=x.indexrelid LEFT JOIN pg_catalog.pg_am am ON am.oid=i.relam WHERE n.nspname='public'`},
	{"indexes", `SELECT i.relname::text,c.relname::text,pg_catalog.jsonb_build_object('definition',(` + indexSemanticsSQL + `)::text,'valid',x.indisvalid,'ready',x.indisready,'live',x.indislive,'unique',x.indisunique,'primary',x.indisprimary,'exclusion',x.indisexclusion)::text FROM pg_catalog.pg_index x JOIN pg_catalog.pg_class i ON i.oid=x.indexrelid JOIN pg_catalog.pg_am am ON am.oid=i.relam JOIN pg_catalog.pg_class c ON c.oid=x.indrelid JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public'`},
	{"triggers", `SELECT t.tgname::text,c.relname::text,pg_catalog.jsonb_build_object('definition',pg_catalog.pg_get_triggerdef(t.oid,false),'enabled',t.tgenabled::text,'internal',t.tgisinternal,'function',fn.nspname||'.'||f.proname||'('||pg_catalog.pg_get_function_identity_arguments(f.oid)||')')::text FROM pg_catalog.pg_trigger t JOIN pg_catalog.pg_class c ON c.oid=t.tgrelid JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace JOIN pg_catalog.pg_proc f ON f.oid=t.tgfoid JOIN pg_catalog.pg_namespace fn ON fn.oid=f.pronamespace WHERE n.nspname='public' AND NOT t.tgisinternal`},
	{"functions", `SELECT p.proname::text,pg_catalog.pg_get_function_identity_arguments(p.oid),pg_catalog.jsonb_build_object('source',p.prosrc,'definition',CASE WHEN p.prokind='a' THEN p.prokind::text||':'||pg_catalog.pg_get_function_result(p.oid) ELSE pg_catalog.pg_get_functiondef(p.oid) END,'security_definer',p.prosecdef,'owner',p.proowner::text,'acl',` + aclExpr("COALESCE(p.proacl,pg_catalog.acldefault('f',p.proowner))") + `,'config',COALESCE(pg_catalog.to_jsonb(p.proconfig),'[]'::jsonb)::text,'volatility',p.provolatile::text,'parallel',p.proparallel::text,'strict',p.proisstrict,'leakproof',p.proleakproof,'language',l.lanname)::text FROM pg_catalog.pg_proc p JOIN pg_catalog.pg_namespace n ON n.oid=p.pronamespace JOIN pg_catalog.pg_language l ON l.oid=p.prolang WHERE n.nspname='public'`},
	{"policies", `SELECT p.polname::text,c.relname::text,pg_catalog.jsonb_build_object('command',p.polcmd::text,'permissive',p.polpermissive,'roles',p.polroles,'using',COALESCE(pg_catalog.pg_get_expr(p.polqual,p.polrelid,false),''),'check',COALESCE(pg_catalog.pg_get_expr(p.polwithcheck,p.polrelid,false),''))::text FROM pg_catalog.pg_policy p JOIN pg_catalog.pg_class c ON c.oid=p.polrelid JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public'`},
	{"sequences", `SELECT c.relname::text,''::text,pg_catalog.jsonb_build_object('type',pg_catalog.format_type(s.seqtypid,NULL),'start',s.seqstart,'increment',s.seqincrement,'min',s.seqmin,'max',s.seqmax,'cache',s.seqcache,'cycle',s.seqcycle,'owner',c.relowner::text,'acl',` + aclExpr("COALESCE(c.relacl,pg_catalog.acldefault('S',c.relowner))") + `,'owned_by',COALESCE((SELECT pg_catalog.jsonb_agg(pg_catalog.jsonb_build_array(rn.nspname,r.relname,a.attname,d.deptype::text) ORDER BY rn.nspname,r.relname,a.attname,d.deptype)::text FROM pg_catalog.pg_depend d JOIN pg_catalog.pg_class r ON r.oid=d.refobjid JOIN pg_catalog.pg_namespace rn ON rn.oid=r.relnamespace JOIN pg_catalog.pg_attribute a ON a.attrelid=r.oid AND a.attnum=d.refobjsubid WHERE d.classid='pg_catalog.pg_class'::regclass AND d.objid=c.oid AND d.refclassid='pg_catalog.pg_class'::regclass AND d.deptype IN ('a','i')),'[]'))::text FROM pg_catalog.pg_sequence s JOIN pg_catalog.pg_class c ON c.oid=s.seqrelid JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public'`},
	{"types", `SELECT t.typname::text,''::text,pg_catalog.jsonb_build_object('type',t.typtype::text,'definition',pg_catalog.jsonb_build_array(t.typtype,t.typcategory,t.typnotnull,pg_catalog.format_type(t.typbasetype,t.typtypmod),COALESCE(t.typdefault,''),t.typlen,t.typbyval,t.typalign,t.typstorage,t.typinput::pg_catalog.regprocedure::text,t.typoutput::pg_catalog.regprocedure::text,pg_catalog.format_type(t.typelem,NULL),
	 COALESCE((SELECT pg_catalog.jsonb_agg(pg_catalog.jsonb_build_array(a.attnum,a.attname,pg_catalog.format_type(a.atttypid,a.atttypmod),a.attnotnull,CASE WHEN a.attcollation=0 THEN '' ELSE a.attcollation::pg_catalog.regcollation::text END) ORDER BY a.attnum) FROM pg_catalog.pg_attribute a WHERE a.attrelid=t.typrelid AND a.attnum>0 AND NOT a.attisdropped),'[]'::jsonb),
	 COALESCE((SELECT pg_catalog.jsonb_build_array(pg_catalog.format_type(r.rngtypid,NULL),pg_catalog.format_type(r.rngmultitypid,NULL),pg_catalog.format_type(r.rngsubtype,NULL),CASE WHEN r.rngcollation=0 THEN '' ELSE r.rngcollation::pg_catalog.regcollation::text END,onsp.nspname||'.'||opc.opcname,r.rngcanonical::pg_catalog.regprocedure::text,r.rngsubdiff::pg_catalog.regprocedure::text) FROM pg_catalog.pg_range r JOIN pg_catalog.pg_opclass opc ON opc.oid=r.rngsubopc JOIN pg_catalog.pg_namespace onsp ON onsp.oid=opc.opcnamespace WHERE r.rngtypid=t.oid OR r.rngmultitypid=t.oid),'[]'::jsonb))::text,
	 'owner',t.typowner::text,'acl',` + aclExpr("COALESCE(t.typacl,pg_catalog.acldefault('T',t.typowner))") + `,'enum',(SELECT COALESCE(pg_catalog.jsonb_agg(e.enumlabel ORDER BY e.enumsortorder),'[]'::jsonb)::text FROM pg_catalog.pg_enum e WHERE e.enumtypid=t.oid))::text FROM pg_catalog.pg_type t JOIN pg_catalog.pg_namespace n ON n.oid=t.typnamespace WHERE n.nspname='public'`},
	{"extensions", `SELECT e.extname::text,''::text,pg_catalog.jsonb_build_object('version',e.extversion,'relocatable',e.extrelocatable,'owner',e.extowner::text)::text FROM pg_catalog.pg_extension e JOIN pg_catalog.pg_namespace n ON n.oid=e.extnamespace WHERE n.nspname='public'`},
}

func aclExpr(expression string) string {
	// expression is a literal from the fixed queries above, never caller input.
	return `(SELECT COALESCE(pg_catalog.jsonb_agg(pg_catalog.jsonb_build_object('grantee',a.grantee::text,'grantor',a.grantor::text,'privilege',a.privilege_type,'grantable',a.is_grantable) ORDER BY a.grantee,a.grantor,a.privilege_type,a.is_grantable),'[]'::jsonb) FROM pg_catalog.aclexplode(CASE WHEN pg_catalog.cardinality(` + expression + `)>0 THEN ` + expression + ` ELSE NULL::pg_catalog.aclitem[] END) a)`
}

// A missing organizations relation remains a missing catalog object; it must
// not turn the whole comparison into an identity-query error.
const identitySQL = `SELECT COALESCE(c.relowner::text,''), r.oid::text, COALESCE(d.datdba=c.relowner,false), COALESCE(n.nspowner=c.relowner,false) FROM pg_catalog.pg_roles r JOIN pg_catalog.pg_database d ON d.datname=pg_catalog.current_database() LEFT JOIN pg_catalog.pg_namespace n ON n.nspname='public' LEFT JOIN pg_catalog.pg_class c ON c.relnamespace=n.oid AND c.relname='organizations' AND c.relkind IN ('r','p') WHERE r.rolname=current_user`

const countsSQL = `SELECT
 (SELECT count(*) FROM pg_catalog.pg_namespace WHERE nspname<>'public'),
 (SELECT count(*) FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname<>'public'),
 (SELECT count(*) FROM pg_catalog.pg_roles WHERE oid::text<>$1 AND oid::text<>$2),
 (SELECT count(*) FROM pg_catalog.pg_default_acl),
 (SELECT count(*) FROM pg_catalog.pg_class),
 (SELECT COALESCE(sum(pg_catalog.cardinality(reloptions)),0) FROM pg_catalog.pg_class),
 (SELECT count(*) FROM pg_catalog.pg_statistic_ext),
 (SELECT count(*) FROM pg_catalog.pg_tablespace)`
