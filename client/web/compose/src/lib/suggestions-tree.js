/* eslint-disable no-template-curly-in-string */
export const getRecordBasedSuggestions = (param = []) => {
  const result = {}

  if ((param || []).some(v => v.interpolate)) {
    result['$'] = []
    result['${'] = []
  }

  (param || []).forEach(({ interpolate, properties, value }) => {
    if (!result.hasOwnProperty('')) result[''] = []

    const prefix = `${interpolate ? '${' : ''}${value}${interpolate && !properties.length ? '}' : ''}`
    const prefixAsValue = `${prefix}${properties.length > 0 ? '.' : ''}`

    result[''].push(prefixAsValue)

    if (interpolate) {
      result['$'].push({
        caption: prefixAsValue.trimStart('$'),
        value: prefixAsValue,
      })
      result['${'].push({
        caption: prefixAsValue.trimStart('${'),
        value: prefixAsValue,
      })
    }

    if (properties.length > 0) {
      result[prefix] = [];

      (properties || []).forEach((prop) => {
        if (typeof prop === 'string') {
          let nestedPrefixValue = prefixAsValue + prop

          result[prefix].push({
            caption: prop,
            value: nestedPrefixValue + (interpolate ? '}' : ''),
          })
        } else {
          let nestedPrefixValue = prefixAsValue + prop.value + '.'
          result[prefix].push({
            caption: prop.value,
            value: nestedPrefixValue,
          })

          result[prefixAsValue + prop.value] = [];

          (prop.properties || []).forEach((v) => {
            let nestedChildPrefixValue = nestedPrefixValue + v

            result[prefixAsValue + prop.value].push({
              caption: v,
              value: nestedChildPrefixValue + (interpolate ? '}' : ''),
            })
          })
        }
      })
    }
  })

  return result
}
