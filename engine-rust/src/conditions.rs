use crate::{
    model::{ensure, ConditionDef, ParamType},
    Result,
};
use cel::{
    common::ast::{Expr, IdedExpr, LiteralValue},
    extractors::This,
    Context, ExecutionError, Program, Value as CelValue,
};
use serde_json::{Map, Value};
use std::{collections::BTreeMap, net::IpAddr, sync::Arc};

#[derive(Clone, Debug, PartialEq)]
enum Ty {
    Bool,
    Int,
    UInt,
    Double,
    String,
    Timestamp,
    Duration,
    Ip,
}
impl Ty {
    fn param(p: &ParamType) -> Result<Self> {
        ensure(
            p.generic_types.is_empty(),
            "Rust CEL generic parameter types are not supported yet",
        )?;
        match p.type_name.as_str().unwrap_or("") {
            "TYPE_NAME_BOOL" => Ok(Self::Bool),
            "TYPE_NAME_INT" => Ok(Self::Int),
            "TYPE_NAME_UINT" => Ok(Self::UInt),
            "TYPE_NAME_DOUBLE" => Ok(Self::Double),
            "TYPE_NAME_STRING" => Ok(Self::String),
            "TYPE_NAME_TIMESTAMP" => Ok(Self::Timestamp),
            "TYPE_NAME_DURATION" => Ok(Self::Duration),
            "TYPE_NAME_IPADDRESS" => Ok(Self::Ip),
            _ => Err("unsupported Rust CEL parameter type".into()),
        }
    }
    fn convert(&self, v: &Value) -> Result<CelValue> {
        let invalid = || format!("invalid {self:?} condition parameter: {v}");
        match self {
            Self::Bool => v.as_bool().map(CelValue::Bool).ok_or_else(invalid),
            Self::String => v
                .as_str()
                .map(|s| CelValue::String(Arc::new(s.into())))
                .ok_or_else(invalid),
            Self::Ip => {
                let ip: IpAddr = v
                    .as_str()
                    .ok_or_else(invalid)?
                    .parse()
                    .map_err(|_| invalid())?;
                Ok(CelValue::String(Arc::new(ip.to_canonical().to_string())))
            }
            Self::Timestamp => {
                let t = chrono::DateTime::parse_from_rfc3339(v.as_str().ok_or_else(invalid)?)
                    .map_err(|_| invalid())?;
                Ok(CelValue::Timestamp(t))
            }
            Self::Duration => {
                let mut ctx = Context::default();
                ctx.add_variable_from_value("value", v.as_str().ok_or_else(invalid)?);
                Program::compile("duration(value)")
                    .map_err(|e| e.to_string())?
                    .execute(&ctx)
                    .map_err(|e| e.to_string())
            }
            Self::Int | Self::UInt | Self::Double => {
                if let Some(text) = v.as_str() {
                    // JSON numbers follow protobuf's float64 conversion. Strings
                    // must retain integer precision beyond 2^53; do not route
                    // them through f64. More numeric spellings stay unsupported.
                    let i = text.parse::<i64>().map_err(|_| {
                        "Rust numeric strings require a signed 64-bit integer spelling".to_string()
                    })?;
                    return match self {
                        Self::Int => Ok(CelValue::Int(i)),
                        Self::UInt => {
                            ensure(i >= 0, "unsigned parameter is negative")?;
                            Ok(CelValue::UInt(i as u64))
                        }
                        Self::Double => {
                            ensure((-9007199254740992..=9007199254740992).contains(&i),"Rust double strings require an exactly representable bounded integer")?;
                            Ok(CelValue::Float(i as f64))
                        }
                        _ => unreachable!(),
                    };
                }
                let x = v
                    .as_f64()
                    .or_else(|| v.as_str().and_then(|s| s.parse::<f64>().ok()))
                    .ok_or_else(invalid)?;
                ensure(x.is_finite(), "nonfinite condition parameter")?;
                if *self == Self::Double {
                    return Ok(CelValue::Float(x));
                }
                ensure(x.fract() == 0., "integer parameter has fractional value")?;
                // OpenFGA converts through float64/big.Float and saturates Int64.
                let i = x as i64;
                if *self == Self::UInt {
                    ensure(i >= 0, "unsigned parameter is negative")?;
                    Ok(CelValue::UInt(i as u64))
                } else {
                    Ok(CelValue::Int(i))
                }
            }
        }
    }
}
pub struct CompiledCondition {
    program: Program,
    parameters: BTreeMap<String, Ty>,
}
impl CompiledCondition {
    pub fn inline(c: &crate::model::Condition) -> Result<Self> {
        let fields = c
            .context
            .as_ref()
            .ok_or("inline expression requires context")?;
        let expression = fields
            .get("expression")
            .and_then(Value::as_str)
            .filter(|s| !s.is_empty())
            .ok_or("inline expression requires a nonempty expression")?;
        ensure(
            fields
                .keys()
                .all(|k| k == "expression" || k == "parameters"),
            "invalid inline expression context key",
        )?;
        let mut parameters = BTreeMap::new();
        if let Some(p) = fields.get("parameters") {
            for (name, ty) in p.as_object().ok_or("inline parameters must be an object")? {
                let ty = ty
                    .as_str()
                    .ok_or("inline parameter type must be a string")?;
                parameters.insert(
                    name.clone(),
                    ParamType {
                        type_name: Value::String(format!("TYPE_NAME_{}", ty.to_uppercase())),
                        generic_types: vec![],
                    },
                );
            }
        }
        let parsed = Program::compile(expression).map_err(|e| e.to_string())?;
        for name in parsed.references().variables() {
            parameters.entry(name.to_string()).or_insert(ParamType {
                type_name: Value::String("TYPE_NAME_STRING".into()),
                generic_types: vec![],
            });
        }
        Self::new(&ConditionDef {
            name: "$expression".into(),
            expression: expression.into(),
            parameters,
        })
    }
    pub fn new(c: &ConditionDef) -> Result<Self> {
        let parameters = c
            .parameters
            .iter()
            .map(|(k, v)| Ok((k.clone(), Ty::param(v)?)))
            .collect::<Result<BTreeMap<_, _>>>()?;
        let program = Program::compile(&c.expression).map_err(|e| e.to_string())?;
        ensure(
            infer(program.expression(), &parameters, 0)? == Ty::Bool,
            "CEL condition must return bool",
        )?;
        Ok(Self {
            program,
            parameters,
        })
    }
    pub fn validate_context(&self, ctx: &Map<String, Value>) -> Result<()> {
        for (k, v) in ctx {
            self.parameters
                .get(k)
                .ok_or_else(|| format!("unknown stored condition parameter: {k}"))?
                .convert(v)?;
        }
        Ok(())
    }
    pub fn evaluate(
        &self,
        request: &Map<String, Value>,
        stored: Option<&Map<String, Value>>,
    ) -> Result<bool> {
        let mut ctx = Context::default();
        ctx.add_function(
            "in_cidr",
            |This(ip): This<Arc<String>>,
             cidr: Arc<String>|
             -> std::result::Result<bool, ExecutionError> {
                let ip: IpAddr = ip
                    .parse()
                    .map_err(|e| ExecutionError::function_error("in_cidr", e))?;
                let mut net: ipnet::IpNet = cidr
                    .parse()
                    .map_err(|e| ExecutionError::function_error("in_cidr", e))?;
                if let ipnet::IpNet::V6(v6) = net {
                    if let Some(v4) = v6.addr().to_ipv4_mapped() {
                        if v6.prefix_len() >= 96 {
                            net = ipnet::Ipv4Net::new(v4, v6.prefix_len() - 96)
                                .map_err(|e| ExecutionError::function_error("in_cidr", e))?
                                .into();
                        }
                    }
                }
                Ok(net.contains(&ip.to_canonical()))
            },
        )
        .map_err(|e| e.to_string())?;
        ctx.add_function(
            "ipaddress",
            |ip: Arc<String>| -> std::result::Result<CelValue, ExecutionError> {
                ip.parse::<IpAddr>()
                    .map(|ip| CelValue::String(Arc::new(ip.to_canonical().to_string())))
                    .map_err(|e| ExecutionError::function_error("ipaddress", e))
            },
        )
        .map_err(|e| e.to_string())?;
        for (name, ty) in &self.parameters {
            let value = stored
                .and_then(|m| m.get(name))
                .or_else(|| request.get(name))
                .ok_or_else(|| format!("missing condition parameter: {name}"))?;
            ctx.add_variable_from_value(name.clone(), ty.convert(value)?);
        }
        match self.program.execute(&ctx).map_err(|e| e.to_string())? {
            CelValue::Bool(b) => Ok(b),
            _ => Err("condition did not return bool".into()),
        }
    }
}
// The Rust CEL library is an interpreter, not the Go CEL type checker. Explicitly
// type-check a conservative scalar subset; reject other AST forms at model load
// instead of accepting unchecked dynamic programs or benchmarking fewer checks.
fn infer(e: &IdedExpr, vars: &BTreeMap<String, Ty>, depth: usize) -> Result<Ty> {
    ensure(depth < 64, "CEL expression nesting limit")?;
    match &e.expr {
        Expr::Ident(name) => vars
            .get(name)
            .cloned()
            .ok_or_else(|| format!("undeclared CEL parameter: {name}")),
        Expr::Literal(v) => match v {
            LiteralValue::Boolean(_) => Ok(Ty::Bool),
            LiteralValue::Int(_) => Ok(Ty::Int),
            LiteralValue::UInt(_) => Ok(Ty::UInt),
            LiteralValue::Double(_) => Ok(Ty::Double),
            LiteralValue::String(_) => Ok(Ty::String),
            _ => Err("unsupported CEL literal".into()),
        },
        Expr::Call(c) => {
            let mut args = Vec::new();
            if let Some(t) = &c.target {
                args.push(infer(t, vars, depth + 1)?)
            }
            for a in &c.args {
                args.push(infer(a, vars, depth + 1)?)
            }
            let is = |types: &[Ty]| args == types;
            match c.func_name.as_str() {
                "_&&_" | "_||_" if is(&[Ty::Bool, Ty::Bool]) => Ok(Ty::Bool),
                "!_" if is(&[Ty::Bool]) => Ok(Ty::Bool),
                "_==_" | "_!=_" if args.len() == 2 && args[0] == args[1] => Ok(Ty::Bool),
                "_<_" | "_>_" | "_<=_" | "_>=_"
                    if args.len() == 2
                        && args[0] == args[1]
                        && matches!(
                            args[0],
                            Ty::Int
                                | Ty::UInt
                                | Ty::Double
                                | Ty::String
                                | Ty::Timestamp
                                | Ty::Duration
                        ) =>
                {
                    Ok(Ty::Bool)
                }
                "_+_" | "_-_" | "_*_" | "_/_" | "_%_"
                    if args.len() == 2
                        && args[0] == args[1]
                        && matches!(args[0], Ty::Int | Ty::UInt | Ty::Double) =>
                {
                    Ok(args[0].clone())
                }
                "-_" if args.len() == 1 && matches!(args[0], Ty::Int | Ty::Double) => {
                    Ok(args[0].clone())
                }
                "_?_:_" if args.len() == 3 && args[0] == Ty::Bool && args[1] == args[2] => {
                    Ok(args[1].clone())
                }
                "timestamp" if is(&[Ty::String]) => Ok(Ty::Timestamp),
                "duration" if is(&[Ty::String]) => Ok(Ty::Duration),
                "ipaddress" if is(&[Ty::String]) => Ok(Ty::Ip),
                "in_cidr" if is(&[Ty::Ip, Ty::String]) => Ok(Ty::Bool),
                "startsWith" | "endsWith" | "contains" | "matches"
                    if is(&[Ty::String, Ty::String]) =>
                {
                    Ok(Ty::Bool)
                }
                _ => Err(format!(
                    "unsupported or ill-typed Rust CEL operation: {}({args:?})",
                    c.func_name
                )),
            }
        }
        _ => Err(
            "Rust CEL currently supports scalar expressions; macros/collections are unsupported"
                .into(),
        ),
    }
}
