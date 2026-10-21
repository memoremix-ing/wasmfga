use crate::{
    model::{object, user, Rewrite, Tuple, ValidatedModel},
    Result,
};
use serde_json::{Map, Value};
use std::collections::{BTreeMap, BTreeSet};

#[derive(Clone, Copy, Debug)]
struct Outcome {
    allowed: bool,
    cycle: bool,
}
impl Outcome {
    fn yes() -> Self {
        Self {
            allowed: true,
            cycle: false,
        }
    }
    fn no() -> Self {
        Self {
            allowed: false,
            cycle: false,
        }
    }
}
type Decision = Result<Outcome>;
fn union(values: impl IntoIterator<Item = Decision>) -> Decision {
    let mut err = None;
    let mut out = Outcome::no();
    for v in values {
        match v {
            Ok(v) if v.allowed => return Ok(v),
            Ok(v) => out.cycle |= v.cycle,
            Err(e) => err = Some(e),
        }
    }
    match err {
        Some(e) => Err(e),
        None => Ok(out),
    }
}
fn intersection(values: impl IntoIterator<Item = Decision>) -> Decision {
    let mut err = None;
    for v in values {
        match v {
            Ok(v) if !v.allowed || v.cycle => {
                return Ok(Outcome {
                    allowed: false,
                    cycle: v.cycle,
                })
            }
            Ok(_) => {}
            Err(e) => err = Some(e),
        }
    }
    match err {
        Some(e) => Err(e),
        None => Ok(Outcome::yes()),
    }
}
pub struct Evaluator<'a> {
    model: &'a ValidatedModel,
    rows: BTreeMap<(String, String), Vec<&'a Tuple>>,
    context: &'a Map<String, Value>,
}
impl<'a> Evaluator<'a> {
    pub fn new(
        model: &'a ValidatedModel,
        stored: &'a [Tuple],
        contextual: &'a [Tuple],
        context: &'a Map<String, Value>,
    ) -> Self {
        let mut seen = BTreeSet::new();
        let mut rows: BTreeMap<(String, String), Vec<&Tuple>> = BTreeMap::new();
        for t in contextual.iter().chain(stored) {
            if seen.insert(t.key()) && model.read_tuple(t).is_ok() {
                rows.entry((t.object.clone(), t.relation.clone()))
                    .or_default()
                    .push(t);
            }
        }
        Self {
            model,
            rows,
            context,
        }
    }
    pub fn check(&self, t: &Tuple) -> Result<bool> {
        self.model.check_key(t)?;
        self.resolve(&t.object, &t.relation, &t.user, &BTreeSet::new(), 0)
            .map(|r| r.allowed)
    }
    fn condition(&self, t: &Tuple) -> Result<bool> {
        match &t.condition {
            None => Ok(true),
            Some(c) if c.name == "$expression" => {
                crate::conditions::CompiledCondition::inline(c)?.evaluate(self.context, None)
            }
            Some(c) => self
                .model
                .compiled
                .get(&c.name)
                .ok_or("condition not defined".into())
                .and_then(|p| p.evaluate(self.context, c.context.as_ref())),
        }
    }
    fn resolve(
        &self,
        obj: &str,
        rel: &str,
        subject: &str,
        path: &BTreeSet<(String, String, String)>,
        depth: usize,
    ) -> Decision {
        if depth >= 25 {
            return Err("resolution depth exceeded".into());
        }
        let key = (obj.into(), rel.into(), subject.into());
        if path.contains(&key) {
            return Ok(Outcome {
                allowed: false,
                cycle: true,
            });
        }
        if subject == format!("{obj}#{rel}") {
            return Ok(Outcome::yes());
        }
        let mut next = path.clone();
        next.insert(key);
        let (ty, _) = object(obj)?;
        let rw = self.model.rewrite(ty, rel)?;
        self.rewrite(obj, rel, subject, rw, &next, depth)
    }
    fn rewrite(
        &self,
        obj: &str,
        rel: &str,
        subject: &str,
        rw: &Rewrite,
        path: &BTreeSet<(String, String, String)>,
        depth: usize,
    ) -> Decision {
        match rw {
            Rewrite::This(_) => {
                let (u, ur) = user(subject)?;
                let (uty, _) = object(u)?;
                let rows = self
                    .rows
                    .get(&(obj.into(), rel.into()))
                    .map(Vec::as_slice)
                    .unwrap_or(&[]);
                union(rows.iter().map(|t| {
                    let (tu, tr) = user(&t.user)?;
                    let (tty, tid) = object(tu)?;
                    let direct = t.user == subject || (tid == "*" && uty == tty && ur.is_none());
                    if direct {
                        return self.condition(t).map(|b| {
                            if b {
                                Outcome::yes()
                            } else {
                                Outcome::no()
                            }
                        });
                    }
                    if let Some(r) = tr {
                        if !self.condition(t)? {
                            return Ok(Outcome::no());
                        }
                        self.resolve(tu, r, subject, path, depth + 1)
                    } else {
                        Ok(Outcome::no())
                    }
                }))
            }
            Rewrite::Computed(c) => self.resolve(obj, &c.relation, subject, path, depth),
            Rewrite::Ttu { tupleset, computed } => {
                let rows = self
                    .rows
                    .get(&(obj.into(), tupleset.relation.clone()))
                    .map(Vec::as_slice)
                    .unwrap_or(&[]);
                union(rows.iter().map(|t| {
                    let (tu, _) = user(&t.user)?;
                    let (ty, _) = object(tu)?;
                    if self.model.rewrite(ty, &computed.relation).is_err() {
                        return Ok(Outcome::no());
                    }
                    if !self.condition(t)? {
                        return Ok(Outcome::no());
                    }
                    self.resolve(tu, &computed.relation, subject, path, depth + 1)
                }))
            }
            Rewrite::Union(c) => union(
                c.child
                    .iter()
                    .map(|rw| self.rewrite(obj, rel, subject, rw, path, depth)),
            ),
            Rewrite::Intersection(c) => intersection(
                c.child
                    .iter()
                    .map(|rw| self.rewrite(obj, rel, subject, rw, path, depth)),
            ),
            Rewrite::Difference { base, subtract } => {
                let b = self.rewrite(obj, rel, subject, base, path, depth);
                if let Ok(v) = &b {
                    if !v.allowed || v.cycle {
                        return Ok(Outcome {
                            allowed: false,
                            cycle: v.cycle,
                        });
                    }
                }
                let s = self.rewrite(obj, rel, subject, subtract, path, depth);
                if let Ok(v) = &s {
                    if v.allowed || v.cycle {
                        return Ok(Outcome {
                            allowed: false,
                            cycle: v.cycle,
                        });
                    }
                }
                b?;
                s?;
                Ok(Outcome::yes())
            }
        }
    }
}
