---
name: data-transformer
description: 数据格式转换工具，支持 JSON/CSV 互转和数据过滤
category: data
language: node
timeout: 15
tags:
  - data
  - transform
  - safe
parameters:
  type: object
  properties:
    data:
      type: string
      description: 输入数据（JSON 数组格式）
    operation:
      type: string
      description: 操作类型（to_csv / filter / stats）
    field:
      type: string
      description: 过滤或统计的字段名
    value:
      type: string
      description: 过滤条件值
  required: [data, operation]
---

# Data Transformer

```node
const args = JSON.parse(process.env.SKILL_ARGS || '{}');
const data = JSON.parse(args.data || '[]');
const op = args.operation;

function toCSV(arr) {
    if (!arr.length) return '';
    const headers = Object.keys(arr[0]);
    const rows = arr.map(item => headers.map(h => JSON.stringify(item[h] ?? '')).join(','));
    return [headers.join(','), ...rows].join('\n');
}

function filter(arr, field, value) {
    return arr.filter(item => String(item[field]) === value);
}

function stats(arr, field) {
    const values = arr.map(item => item[field]).filter(v => typeof v === 'number');
    if (!values.length) return { error: 'no numeric values found' };
    const sum = values.reduce((a, b) => a + b, 0);
    return {
        count: values.length,
        sum,
        avg: (sum / values.length).toFixed(2),
        min: Math.min(...values),
        max: Math.max(...values),
    };
}

let result;
switch (op) {
    case 'to_csv': result = toCSV(data); break;
    case 'filter': result = filter(data, args.field, args.value); break;
    case 'stats': result = stats(data, args.field); break;
    default: result = { error: `unknown operation: ${op}` };
}

console.log(typeof result === 'string' ? result : JSON.stringify(result, null, 2));
```