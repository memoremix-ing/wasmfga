use crate::{conditions::CompiledCondition, Result};
use serde::{Deserialize, Serialize};
use serde_json::{Map, Value};
use std::collections::{BTreeMap, BTreeSet};

#[derive(Clone, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub struct Model {
    #[serde(default)]
    pub id: String,
    #[serde(alias = "schemaVersion")]
    pub schema_version: String,
    #[serde(alias = "typeDefinitions")]
    pub type_definitions: Vec<TypeDef>,
    #[serde(default)]
    pub conditions: BTreeMap<String, ConditionDef>,
}
#[derive(Clone, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub struct TypeDef {
    #[serde(rename = "type")]
    pub name: String,
    #[serde(default)]
    pub relations: BTreeMap<String, Rewrite>,
    #[serde(default)]
    pub metadata: Metadata,
}
#[derive(Clone, Default, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub struct Metadata {
    #[serde(default)]
    pub relations: BTreeMap<String, RelationMetadata>,
}
#[derive(Clone, Default, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub struct RelationMetadata {
    #[serde(default, alias = "directlyRelatedUserTypes")]
    pub directly_related_user_types: Vec<Restriction>,
}
#[derive(Clone, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub struct Restriction {
    #[serde(rename = "type")]
    pub name: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub relation: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub wildcard: Option<Empty>,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub condition: String,
}
#[derive(Clone, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub struct Empty {}
#[derive(Clone, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub struct Rel {
    pub relation: String,
    #[serde(default)]
    pub object: String,
}
#[derive(Clone, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub struct Children {
    pub child: Vec<Rewrite>,
}
#[derive(Clone, Deserialize, Serialize)]
pub enum Rewrite {
    #[serde(rename = "this")]
    This(Empty),
    #[serde(rename = "computedUserset", alias = "computed_userset")]
    Computed(Rel),
    #[serde(rename = "tupleToUserset", alias = "tuple_to_userset")]
    Ttu {
        tupleset: Rel,
        #[serde(rename = "computedUserset", alias = "computed_userset")]
        computed: Rel,
    },
    #[serde(rename = "union")]
    Union(Children),
    #[serde(rename = "intersection")]
    Intersection(Children),
    #[serde(rename = "difference")]
    Difference {
        base: Box<Rewrite>,
        subtract: Box<Rewrite>,
    },
}
#[derive(Clone, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub struct ConditionDef {
    pub name: String,
    pub expression: String,
    #[serde(default)]
    pub parameters: BTreeMap<String, ParamType>,
}
#[derive(Clone, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub struct ParamType {
    #[serde(alias = "typeName")]
    pub type_name: Value,
    #[serde(default, alias = "genericTypes")]
    pub generic_types: Vec<ParamType>,
}
#[derive(Clone, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub struct Tuple {
    pub object: String,
    pub relation: String,
    pub user: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub condition: Option<Condition>,
}
#[derive(Clone, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub struct Condition {
    pub name: String,
    #[serde(default)]
    pub context: Option<Map<String, Value>>,
}
impl Tuple {
    pub fn key(&self) -> (&str, &str, &str) {
        (&self.object, &self.relation, &self.user)
    }
}

pub struct ValidatedModel {
    pub model: Model,
    pub compiled: BTreeMap<String, CompiledCondition>,
    pub tuplesets: BTreeSet<(String, String)>,
    indexes: BTreeMap<String, usize>,
}
fn identifier(s: &str) -> bool {
    !s.is_empty() && s.len() <= 254 && s.chars().all(|c| c.is_ascii_alphanumeric() || c == '_')
}
pub fn object(s: &str) -> Result<(&str, &str)> {
    let (ty, id) = s.split_once(':').ok_or("object must be type:id")?;
    ensure(
        identifier(ty)
            && !id.is_empty()
            && id.len() <= 1024
            && !id
                .chars()
                .any(|c| c == ':' || c == '#' || c.is_whitespace() || c.is_control()),
        "malformed object",
    )?;
    Ok((ty, id))
}
pub fn user(s: &str) -> Result<(&str, Option<&str>)> {
    let (obj, rel) = match s.split_once('#') {
        Some((o, r)) => {
            ensure(identifier(r), "malformed userset")?;
            (o, Some(r))
        }
        None => (s, None),
    };
    let (_, id) = object(obj)?;
    ensure(!(id == "*" && rel.is_some()), "wildcard userset")?;
    Ok((obj, rel))
}
pub fn ensure(ok: bool, message: &str) -> Result<()> {
    if ok {
        Ok(())
    } else {
        Err(message.into())
    }
}
pub fn validate_context(v: &Value) -> Result<()> {
    match v {
        Value::String(s) => ensure(
            !s.chars().any(|c| c.is_control()),
            "forbidden context character",
        ),
        Value::Object(m) => {
            for (k, v) in m {
                validate_context(&Value::String(k.clone()))?;
                validate_context(v)?;
            }
            Ok(())
        }
        Value::Array(a) => {
            for v in a {
                validate_context(v)?;
            }
            Ok(())
        }
        _ => Ok(()),
    }
}
impl ValidatedModel {
    pub fn new(model: Model) -> Result<Self> {
        ensure(
            model.schema_version == "1.1",
            "Rust evaluator supports schema 1.1",
        )?;
        ensure(
            !model.type_definitions.is_empty(),
            "model requires type definitions",
        )?;
        let mut indexes = BTreeMap::new();
        for (i, t) in model.type_definitions.iter().enumerate() {
            ensure(
                identifier(&t.name) && !indexes.contains_key(&t.name),
                "invalid/duplicate type",
            )?;
            indexes.insert(t.name.clone(), i);
        }
        let mut compiled = BTreeMap::new();
        for (name, c) in &model.conditions {
            ensure(
                identifier(name) && name == &c.name,
                "invalid condition name",
            )?;
            compiled.insert(name.clone(), CompiledCondition::new(c)?);
        }
        let mut m = Self {
            model,
            compiled,
            tuplesets: BTreeSet::new(),
            indexes,
        };
        let mut tuplesets = BTreeSet::new();
        for t in &m.model.type_definitions {
            for (name, rw) in &t.relations {
                ensure(identifier(name), "invalid relation name")?;
                m.validate_rewrite(&t.name, rw, &mut tuplesets, 0)?;
                let restrictions = m.restrictions(&t.name, name);
                ensure(
                    contains_this(rw) != restrictions.is_empty(),
                    "direct type restrictions must match an assignable rewrite",
                )?;
                for r in restrictions {
                    m.type_def(&r.name)?;
                    ensure(
                        r.wildcard.is_none() || r.relation.is_empty(),
                        "invalid wildcard restriction",
                    )?;
                    if !r.relation.is_empty() {
                        m.rewrite(&r.name, &r.relation)?;
                    }
                    ensure(
                        r.condition.is_empty()
                            || r.condition == "$expression"
                            || m.compiled.contains_key(&r.condition),
                        "unknown or unsupported condition",
                    )?;
                }
            }
            for name in t.metadata.relations.keys() {
                ensure(
                    t.relations.contains_key(name),
                    "metadata for unknown relation",
                )?;
            }
        }
        m.tuplesets = tuplesets;
        for (ty, rel) in &m.tuplesets {
            ensure(
                matches!(m.rewrite(ty, rel)?, Rewrite::This(_)),
                "tupleset must be a direct relation",
            )?;
            for r in m.restrictions(ty, rel) {
                ensure(
                    r.wildcard.is_none() && r.relation.is_empty(),
                    "tupleset must refer to concrete objects",
                )?;
            }
        }
        for t in &m.model.type_definitions {
            for (name, rw) in &t.relations {
                let mut path = BTreeSet::new();
                path.insert(name.clone());
                m.computed_cycle(&t.name, rw, &path, 0)?;
            }
        }
        // A least fixed point rejects ungrounded relation cycles, while allowing
        // recursive usersets/parents with a concrete entrypoint.
        let mut grounded = BTreeSet::new();
        loop {
            let mut next = grounded.clone();
            for t in &m.model.type_definitions {
                for (name, rw) in &t.relations {
                    if m.grounded(&t.name, name, rw, &grounded) {
                        next.insert((t.name.clone(), name.clone()));
                    }
                }
            }
            if next == grounded {
                break;
            }
            grounded = next;
        }
        for t in &m.model.type_definitions {
            for name in t.relations.keys() {
                ensure(
                    grounded.contains(&(t.name.clone(), name.clone())),
                    "relation has no entrypoint",
                )?;
            }
        }
        Ok(m)
    }
    pub fn type_def(&self, name: &str) -> Result<&TypeDef> {
        self.indexes
            .get(name)
            .map(|i| &self.model.type_definitions[*i])
            .ok_or_else(|| format!("type '{name}' not found"))
    }
    pub fn rewrite(&self, ty: &str, rel: &str) -> Result<&Rewrite> {
        self.type_def(ty)?
            .relations
            .get(rel)
            .ok_or_else(|| format!("relation '{ty}#{rel}' not found"))
    }
    pub fn restrictions(&self, ty: &str, rel: &str) -> &[Restriction] {
        self.type_def(ty)
            .ok()
            .and_then(|t| t.metadata.relations.get(rel))
            .map(|m| m.directly_related_user_types.as_slice())
            .unwrap_or(&[])
    }
    fn validate_rewrite(
        &self,
        ty: &str,
        r: &Rewrite,
        sets: &mut BTreeSet<(String, String)>,
        depth: usize,
    ) -> Result<()> {
        ensure(depth < 64, "rewrite nesting limit exceeded")?;
        match r {
            Rewrite::This(_) => Ok(()),
            Rewrite::Computed(c) => {
                ensure(c.object.is_empty(), "computed object must be empty")?;
                self.rewrite(ty, &c.relation)?;
                Ok(())
            }
            Rewrite::Ttu { tupleset, computed } => {
                ensure(
                    tupleset.object.is_empty() && computed.object.is_empty(),
                    "TTU object must be empty",
                )?;
                self.rewrite(ty, &tupleset.relation)?;
                sets.insert((ty.into(), tupleset.relation.clone()));
                ensure(
                    self.restrictions(ty, &tupleset.relation)
                        .iter()
                        .any(|t| self.rewrite(&t.name, &computed.relation).is_ok()),
                    "TTU computed relation is undefined",
                )
            }
            Rewrite::Union(c) | Rewrite::Intersection(c) => {
                ensure(
                    c.child.len() >= 2,
                    "set operation requires at least two children",
                )?;
                for x in &c.child {
                    self.validate_rewrite(ty, x, sets, depth + 1)?
                }
                Ok(())
            }
            Rewrite::Difference { base, subtract } => {
                self.validate_rewrite(ty, base, sets, depth + 1)?;
                self.validate_rewrite(ty, subtract, sets, depth + 1)
            }
        }
    }
    fn computed_cycle(
        &self,
        ty: &str,
        rw: &Rewrite,
        path: &BTreeSet<String>,
        depth: usize,
    ) -> Result<()> {
        ensure(depth < 256, "computed rewrite depth limit exceeded")?;
        match rw {
            Rewrite::Computed(c) => {
                ensure(!path.contains(&c.relation), "computed userset cycle")?;
                let mut next = path.clone();
                next.insert(c.relation.clone());
                self.computed_cycle(ty, self.rewrite(ty, &c.relation)?, &next, depth + 1)
            }
            Rewrite::Union(c) | Rewrite::Intersection(c) => {
                for r in &c.child {
                    self.computed_cycle(ty, r, path, depth + 1)?;
                }
                Ok(())
            }
            Rewrite::Difference { base, subtract } => {
                self.computed_cycle(ty, base, path, depth + 1)?;
                self.computed_cycle(ty, subtract, path, depth + 1)
            }
            _ => Ok(()),
        }
    }
    fn grounded(&self, ty: &str, rel: &str, r: &Rewrite, g: &BTreeSet<(String, String)>) -> bool {
        match r {
            Rewrite::This(_) => self.restrictions(ty, rel).iter().any(|r| {
                r.relation.is_empty() || g.contains(&(r.name.clone(), r.relation.clone()))
            }),
            Rewrite::Computed(r) => g.contains(&(ty.into(), r.relation.clone())),
            Rewrite::Ttu { tupleset, computed } => self
                .restrictions(ty, &tupleset.relation)
                .iter()
                .any(|r| g.contains(&(r.name.clone(), computed.relation.clone()))),
            Rewrite::Union(c) => c.child.iter().any(|r| self.grounded(ty, rel, r, g)),
            Rewrite::Intersection(c) => c.child.iter().all(|r| self.grounded(ty, rel, r, g)),
            Rewrite::Difference { base, .. } => self.grounded(ty, rel, base, g),
        }
    }
    pub fn check_key(&self, t: &Tuple) -> Result<()> {
        let (ty, id) = object(&t.object)?;
        ensure(id != "*", "object cannot be wildcard")?;
        ensure(identifier(&t.relation), "invalid relation")?;
        self.rewrite(ty, &t.relation)?;
        let (obj, rel) = user(&t.user)?;
        let (uty, _) = object(obj)?;
        self.type_def(uty)?;
        if let Some(r) = rel {
            self.rewrite(uty, r)?;
        }
        Ok(())
    }
    pub fn tuple(&self, t: &Tuple) -> Result<()> {
        self.tuple_inner(t, true)
    }
    pub fn read_tuple(&self, t: &Tuple) -> Result<()> {
        self.tuple_inner(t, false)
    }
    fn tuple_inner(&self, t: &Tuple, for_write: bool) -> Result<()> {
        self.check_key(t)?;
        let (ty, _) = object(&t.object)?;
        let (u, ur) = user(&t.user)?;
        let (uty, uid) = object(u)?;
        if self.tuplesets.contains(&(ty.into(), t.relation.clone())) {
            ensure(ur.is_none() && uid != "*", "invalid tupleset user")?;
        }
        let cond = t.condition.as_ref().map(|c| c.name.as_str()).unwrap_or("");
        let valid = self.restrictions(ty, &t.relation).iter().any(|r| {
            r.name == uty
                && r.relation == ur.unwrap_or("")
                && r.wildcard.is_some() == (uid == "*")
                && r.condition == cond
        });
        ensure(valid, "tuple does not match a type/condition restriction")?;
        if let Some(c) = &t.condition {
            ensure(!c.name.is_empty(), "condition name is required")?;
            if c.name == "$expression" {
                if let Some(ctx) = &c.context {
                    validate_context(&Value::Object(ctx.clone()))?;
                }
                // Read filters must never drop a valid but unsupported inline
                // expression: doing so in an exclusion could turn a deny into an
                // allow. Keep it for evaluation, which returns a terminal error.
                if for_write {
                    CompiledCondition::inline(c)?;
                }
                return Ok(());
            }
            let compiled = self.compiled.get(&c.name).ok_or("undefined condition")?;
            if let Some(ctx) = &c.context {
                validate_context(&Value::Object(ctx.clone()))?;
                compiled.validate_context(ctx)?;
            }
        }
        Ok(())
    }
}
pub fn contains_this(r: &Rewrite) -> bool {
    match r {
        Rewrite::This(_) => true,
        Rewrite::Union(c) | Rewrite::Intersection(c) => c.child.iter().any(contains_this),
        Rewrite::Difference { base, subtract } => contains_this(base) || contains_this(subtract),
        _ => false,
    }
}
