"""Pure synthetic source bytes; no database or repository adapters."""
from dataclasses import FrozenInstanceError
from pathlib import Path
import copy
import json
import unittest

import pending_plan as p

H = "a" * 64
EMPTY = p.digest(b"")


def obj(kind, name, attrs, parent=""):
    result = {"kind":kind,"identity":name,"attributes":attrs}
    if parent: result["parent"]=parent
    return result


def relation(name):
    return obj("relations",name,dict(relkind="r",rls="false",force_rls="false",
        owner="MIGRATION_OWNER",acl="MIGRATION_OWNER>RUNTIME:SELECT:false",security_invoker="false",
        view_sha256=EMPTY,partition_key_sha256=EMPTY,partition_bound_sha256=EMPTY,parents_sha256=H))


def column(table,name,position):
    return obj("columns",table+"."+name,dict(position=str(position),type_sha256=H,not_null="true",
        default_sha256=EMPTY,identity="",generated="",collation_sha256=EMPTY,acl=""),table)


def function(name):
    return obj("functions",name+"("+EMPTY+")",dict(source_sha256=H,definition_sha256=H,
        security_definer="false",owner="MIGRATION_OWNER",acl="MIGRATION_OWNER>RUNTIME:EXECUTE:false",
        config_sha256=H,volatility="s",parallel="u",strict="false",leakproof="false",language_sha256=H))


def table_group(name,columns):
    out=[relation(name),*(column(name,col,i+1) for i,col in enumerate(columns))]
    for type_name,type_kind in ((name,"c"),("_"+name,"b")):
        out.append(obj("types",type_name,dict(type=type_kind,definition_sha256=H,
            owner="MIGRATION_OWNER",acl="",enum_sha256=EMPTY)))
    out += [obj("constraints",name+".pk",dict(type="p",definition_sha256=H,validated="true",deferrable="false",deferred="false"),name),
        obj("indexes",name+".pk",dict(definition_sha256=H,valid="true",ready="true",live="true",unique="true",primary="true",exclusion="false"),name)]
    return out


def ref(row):
    return {k:row.get(k,"") for k in ("kind","identity","parent")}


def fixture():
    active={"version":0,"objects":[relation("organizations"),column("organizations","id",1)]}
    globals_=[{"name":"organizations","reason":"Synthetic fixture identity root."}]
    state=table_group("rereply_schema_state",["catalog_version","min_reader_version","catalog_sha256","applied_at"])
    state += [function("rereply_schema_state_monotonic"),obj("triggers","rereply_schema_state.monotonic",
        dict(definition_sha256=H,enabled="O",internal="false",function_sha256=H),"rereply_schema_state")]
    ledger=table_group("rereply_data_steps",["step_id","revision","applied_at"])
    catalog_fn=[function("rereply_schema_catalog_version")]
    target={"version":1,"objects":active["objects"]+state+ledger+catalog_fn}
    globals_ += [{"name":"rereply_schema_state","reason":"Synthetic global schema state."},
                 {"name":"rereply_data_steps","reason":"Synthetic global migration ledger."}]
    steps=[]
    for ident,operation,objects,deps,gates in (
        ("create_state","create_relation",state,[],[]),
        ("create_ledger","create_relation",ledger,[],["schema_ready"]),
        ("create_catalog_function","create_function",catalog_fn,["create_state"],[]),
        ("grant_runtime","grant",[state[0],ledger[0],catalog_fn[0]],
         ["create_catalog_function","create_ledger","create_state"],[])):
        steps.append(dict(id=ident,operation=operation,objects=sorted(map(ref,objects),key=lambda x:(x["kind"],x["identity"],x["parent"])),
            depends_on=deps,requires_gates=gates,source_sha256=H))
    history=[{"version":0,"class":"baseline","min_reader_version":0}]
    args=dict(active_catalog=p.canonical(active),active_globals=p.canonical(globals_[:1]),active_history=p.canonical(history))
    target_file=p.PREFIX+"catalog-v1.json"
    files={target_file:p.canonical(target),p.GLOBALS:p.canonical(globals_)}
    plan=dict(format_version=1,active_version=0,
        active_catalog_file_sha256=p.digest(args["active_catalog"]),
        active_globals_file_sha256=p.digest(args["active_globals"]),
        active_history_file_sha256=p.digest(args["active_history"]),globals_file_sha256=p.digest(files[p.GLOBALS]),
        entries=[dict(version=1,**{"class":"expand"},min_reader_version=0,pending=True,catalog_file=target_file,
            catalog_file_sha256=p.digest(files[target_file]),globals_sha256=p.digest(files[p.GLOBALS]),
            steps=["create_state","create_ledger","create_catalog_function","grant_runtime"])],
        steps=sorted(steps,key=lambda s:s["id"]))
    files[p.PLAN]=p.canonical(plan)
    return {**args,"files":files}


class PendingTests(unittest.TestCase):
    def setUp(self): self.args=fixture()

    def plan(self): return json.loads(self.args["files"][p.PLAN])

    def update_plan(self,change):
        doc=self.plan();change(doc);self.args["files"][p.PLAN]=p.canonical(doc)

    def update_target(self,change):
        name=p.PREFIX+"catalog-v1.json"
        doc=json.loads(self.args["files"][name]);change(doc)
        self.args["files"][name]=p.canonical(doc)
        self.update_plan(lambda plan:plan["entries"][0].update(catalog_file_sha256=p.digest(self.args["files"][name])))

    def reject(self,code=None):
        with self.assertRaises(p.PendingRefusal) as caught: p.parse_bundle(**self.args)
        self.assertTrue(str(caught.exception).startswith("pending-invalid:"))
        if code:self.assertEqual(str(caught.exception),"pending-invalid:"+code)

    def test_pr22_shape_preserves_four_steps_global_relations_invoker_and_gate(self):
        result=p.parse_bundle(**self.args)
        self.assertEqual(result.active_version,0)
        self.assertEqual(result.entries[0].version,1)
        self.assertEqual(result.entries[0].steps,("create_state","create_ledger","create_catalog_function","grant_runtime"))
        self.assertEqual([t.name for t in result.entries[0].globals],
            ["organizations","rereply_data_steps","rereply_schema_state"])
        self.assertEqual(next(s for s in result.steps if s.id=="create_ledger").requires_gates,("schema_ready",))
        fn=next(o for o in result.entries[0].objects if o.ref.identity.startswith("rereply_schema_catalog_version("))
        self.assertEqual(dict(fn.attributes)["security_definer"],"false")

    def test_output_is_deeply_immutable_and_input_mutation_cannot_change_it(self):
        result=p.parse_bundle(**self.args)
        with self.assertRaises(FrozenInstanceError): result.active_version=1
        with self.assertRaises(TypeError): result.steps[0].objects[0]=None
        self.args["files"].clear()
        self.assertEqual(len(result.files),3)

    def test_prior_identical_bundle_is_accepted(self):
        self.assertEqual(p.parse_bundle(**self.args),p.parse_bundle(**self.args,prior_files=copy.deepcopy(self.args["files"])))

    def test_valid_replacement_is_refused_while_active_bytes_unchanged(self):
        prior=copy.deepcopy(self.args["files"])
        self.update_plan(lambda d:d["steps"][0].update(source_sha256="b"*64))
        p.parse_bundle(**self.args)
        with self.assertRaisesRegex(p.PendingRefusal,"^pending-invalid:prior-bundle-changed$"):
            p.parse_bundle(**self.args,prior_files=prior)

    def test_prior_is_reparsed_not_trusted_as_a_dataclass(self):
        with self.assertRaisesRegex(p.PendingRefusal,"^pending-invalid:bundle$"):
            p.parse_bundle(**self.args,prior_files=p.parse_bundle(**self.args))

    def test_active_changed_cannot_consume_old_pending(self):
        prior=copy.deepcopy(self.args["files"])
        self.args["active_history"]+=b" "
        self.update_plan(lambda d:d.update(active_history_file_sha256=p.digest(self.args["active_history"])))
        with self.assertRaisesRegex(p.PendingRefusal,"^pending-invalid:binding$"):
            p.parse_bundle(**self.args,prior_files=prior)

    def test_active_whitespace_is_not_rewritten_and_is_bound_exactly(self):
        for key in ("active_catalog","active_globals","active_history"):
            self.args[key]=json.dumps(json.loads(self.args[key]),indent=2).encode()+b"\n"
        self.update_plan(lambda d:d.update(**{key+"_file_sha256":p.digest(self.args[key]) for key in
            ("active_catalog","active_globals","active_history")}))
        result=p.parse_bundle(**self.args)
        self.assertEqual(result.active_catalog_file_sha256,p.digest(self.args["active_catalog"]))

    def test_noncanonical_pending_and_trailing_json_refuse(self):
        for raw in (json.dumps(self.plan(),indent=2).encode(),self.args["files"][p.PLAN]+b"{}"):
            with self.subTest(raw_length=len(raw)):
                self.args["files"][p.PLAN]=raw;self.reject()

    def test_duplicate_keys_and_nonfinite_numbers_refuse(self):
        for raw in (b'{"format_version":1,"format_version":1}',b'{"x":NaN}',b'{"x":Infinity}',b'{"x":1.5}'):
            with self.subTest(raw=raw):self.args["files"][p.PLAN]=raw;self.reject()

    def test_exact_keys_boolean_versions_and_unconfigured_bindings_refuse(self):
        for change in (lambda d:d.update(executed=True),lambda d:d.update(format_version=True),
                       lambda d:d.update(active_version=False),lambda d:d.update(globals_file_sha256="A"*64),
                       lambda d:d.update(active_history_file_sha256="b"*64)):
            self.args=fixture();self.update_plan(change);self.reject()

    def test_extra_missing_and_unsafe_filenames_refuse(self):
        for change in (lambda f:f.update({p.PREFIX+"unknown.json":b"{}\n"}),lambda f:f.pop(p.GLOBALS),
                       lambda f:f.update({p.PREFIX+"../catalog-v1.json":b"{}\n"})):
            self.args=fixture();change(self.args["files"]);self.reject()

    def test_filename_must_match_exact_active_plus_one(self):
        for name in (p.PREFIX+"catalog-v01.json",p.PREFIX+"catalog-v2.json","catalog-v1.json"):
            self.args=fixture();self.update_plan(lambda d:d["entries"][0].update(catalog_file=name));self.reject("catalog-binding")

    def test_multiple_targets_and_empty_steps_refuse(self):
        for change in (lambda d:d["entries"].append(copy.deepcopy(d["entries"][0])),lambda d:d.update(steps=[])):
            self.args=fixture();self.update_plan(change);self.reject("plan-count")

    def test_version_pending_flag_and_reader_floor_rules(self):
        for change in (dict(version=0),dict(version=2),dict(pending=False),dict(pending=1),
                       dict(min_reader_version=1),dict(min_reader_version=-1),{"class":"baseline"}):
            self.args=fixture();self.update_plan(lambda d:d["entries"][0].update(change));self.reject("entry")
        self.args=fixture();self.update_plan(lambda d:d["entries"][0].update({"class":"contract","min_reader_version":1}))
        self.assertEqual(p.parse_bundle(**self.args).entries[0].min_reader_version,1)

    def test_active_engine_and_history_floor_refuse(self):
        self.args["active_history"]=p.canonical([dict(version=0,**{"class":"baseline"},min_reader_version=0,apply_engine=True)])
        self.reject("active-engine")

    def test_target_and_globals_hash_tampering_refuse(self):
        for name in (p.PREFIX+"catalog-v1.json",p.GLOBALS):
            self.args=fixture();self.args["files"][name]+=b" ";self.reject()

    def test_unknown_operation_and_empty_or_private_ids_refuse_without_echo(self):
        for change in (dict(operation="run_sql"),dict(id=""),dict(id="PRIVATE/secret-token"),dict(source_sha256="bad")):
            self.args=fixture();self.update_plan(lambda d:d["steps"][0].update(change))
            with self.assertRaises(p.PendingRefusal) as caught:p.parse_bundle(**self.args)
            self.assertNotIn("PRIVATE",str(caught.exception))

    def test_duplicate_step_and_unused_step_refuse(self):
        self.update_plan(lambda d:d["steps"].append(copy.deepcopy(d["steps"][0])));self.reject("step-duplicate-order")
        self.args=fixture()
        def unused(d):
            s=copy.deepcopy(d["steps"][0]);s["id"]="unused";d["steps"].append(s);d["steps"].sort(key=lambda s:s["id"])
        self.update_plan(unused);self.reject("bundle-extra")

    def test_dependency_cycles_unknown_and_wrong_order_refuse(self):
        for dependencies in (["create_state"],["grant_runtime"],["unknown"]):
            self.args=fixture()
            self.update_plan(lambda d:next(s for s in d["steps"] if s["id"]=="create_state").update(depends_on=dependencies))
            self.reject("dependency-order")

    def test_repeated_entry_step_and_missing_step_refuse(self):
        for ids in (["create_state","create_state"],["unknown"]):
            self.args=fixture();self.update_plan(lambda d:d["entries"][0].update(steps=ids));self.reject("entry-steps")

    def test_gate_names_are_sorted_unique_bounded_source_identifiers(self):
        for gates in (["z","a"],["a","a"],["private/path"],["a"]*33):
            self.args=fixture();self.update_plan(lambda d:d["steps"][0].update(requires_gates=gates));self.reject("ids")

    def test_wrong_parent_unknown_reference_and_duplicate_reference_refuse(self):
        for change in (lambda s:s["objects"][0].update(parent="other"),
                       lambda s:s["objects"].append(dict(kind="relations",identity="missing",parent="")),
                       lambda s:s["objects"].append(copy.deepcopy(s["objects"][0]))):
            self.args=fixture();self.update_plan(lambda d:change(d["steps"][0]));self.reject()

    def test_changed_catalog_companion_cannot_be_omitted_from_plan(self):
        self.update_target(lambda d:d["objects"].append(obj("types","surprise",dict(type="e",definition_sha256=H,
            owner="MIGRATION_OWNER",acl="",enum_sha256=H))))
        self.reject("uncovered-delta")

    def test_unbound_global_or_seed_key_refuses(self):
        for row in (dict(name="missing",reason="Synthetic"),dict(name="rereply_schema_state",reason="Synthetic",keys=["missing"])):
            self.args=fixture();raw=p.canonical([row]);self.args["files"][p.GLOBALS]=raw
            self.update_plan(lambda d:(d.update(globals_file_sha256=p.digest(raw)),d["entries"][0].update(globals_sha256=p.digest(raw))))
            self.reject()

    def test_duplicate_object_unknown_attribute_and_unbound_child_refuse(self):
        for change in (lambda d:d["objects"].append(copy.deepcopy(d["objects"][0])),
                       lambda d:d["objects"][0]["attributes"].update(unreviewed="true"),
                       lambda d:d["objects"].append(column("missing","id",1))):
            self.args=fixture();self.update_target(change);self.reject()

    def test_byte_depth_and_count_bounds_refuse(self):
        for raw in (b" "*(p.MAX_FILE_BYTES+1),b"["*18+b"0"+b"]"*18):
            self.args=fixture();self.args["files"][p.PLAN]=raw;self.reject()
        self.args=fixture();self.update_plan(lambda d:d.update(steps=d["steps"]*(p.MAX_STEPS+1)));self.reject("plan-count")

    def test_declared_concurrent_operation_does_not_create_execution_proof(self):
        self.update_plan(lambda d:d["steps"][0].update(operation="create_index_concurrently"))
        result=p.parse_bundle(**self.args)
        self.assertEqual(result.steps[0].operation,"create_index_concurrently")
        # Deliberately no semantic classification: source operation consistency
        # requires the future typed compiler/real capture, outside this parser.

    def test_complete_existing_v0_source_bytes_remain_unchanged_and_accepted(self):
        root=Path(__file__).resolve().parents[2]/"internal"/"dbcatalog"/"golden"
        names={"active_catalog":"catalog.json", "active_globals":"global_tables.json",
               "active_history":"history.json"}
        original={key:(root/name).read_bytes() for key,name in names.items()}
        baseline=json.loads(original["active_catalog"])
        target=json.loads(self.args["files"][p.PREFIX+"catalog-v1.json"])
        target["objects"]=[*baseline["objects"],*(row for row in target["objects"]
            if row["identity"] not in ("organizations","organizations.id"))]
        globals_=json.loads(original["active_globals"])
        globals_ += [row for row in json.loads(self.args["files"][p.GLOBALS]) if row["name"]!="organizations"]
        self.args.update(original)
        name=p.PREFIX+"catalog-v1.json"
        self.args["files"][name]=p.canonical(target)
        self.args["files"][p.GLOBALS]=p.canonical(globals_)
        def bind(doc):
            doc.update(**{key+"_file_sha256":p.digest(raw) for key,raw in original.items()},
                globals_file_sha256=p.digest(self.args["files"][p.GLOBALS]))
            doc["entries"][0].update(catalog_file_sha256=p.digest(self.args["files"][name]),
                globals_sha256=p.digest(self.args["files"][p.GLOBALS]))
        self.update_plan(bind)
        result=p.parse_bundle(**self.args)
        self.assertGreater(len(result.entries[0].objects),4000)
        self.assertEqual(result.active_catalog_file_sha256,p.digest(original["active_catalog"]))
        self.assertEqual({key:(root/name).read_bytes() for key,name in names.items()},original)

    def test_active_reader_floor_is_monotone_and_only_contract_can_raise_it(self):
        baseline=json.loads(self.args["active_history"])
        for rows in (
            baseline+[dict(version=1,**{"class":"expand"},min_reader_version=1)],
            baseline+[dict(version=1,**{"class":"contract"},min_reader_version=1),
                      dict(version=2,**{"class":"expand"},min_reader_version=0)],
        ):
            self.args["active_history"]=p.canonical(rows)
            self.reject("active-history")


if __name__=="__main__":unittest.main()
