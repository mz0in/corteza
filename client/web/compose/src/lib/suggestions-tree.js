/* eslint-disable no-template-curly-in-string */
export const getRecordBasedSuggestions = (params) => {
  const result = {}

  function addSuggestion (key, caption, value) {
    if (!result[key]) result[key] = []
    result[key].push({ caption: caption, value: value })
  }

  function processProperties (prefix, properties, interpolate) {
    (properties || []).forEach((prop) => {
      if (typeof prop === 'string') {
        let value = prefix + '.' + prop + (interpolate ? '}' : '')
        addSuggestion(prefix, prop, value)
      } else {
        let nestedPrefix = prefix + '.' + prop.value + '.'
        addSuggestion(prefix, prop.value, nestedPrefix)

        if (prop.properties) {
          (prop.properties || []).forEach((nestedProp) => {
            let nestedValue = nestedPrefix + nestedProp + (interpolate ? '}' : '')
            addSuggestion(prefix + '.' + prop.value, nestedProp, nestedValue)
          })
        }
      }
    })
  }

  (params || []).forEach((p) => {
    if (typeof p === 'string') {
      addSuggestion('', '', p)
    } else {
      const { interpolate = false, properties = [], value, root = true } = p
      let prefix = interpolate ? '${' : ''
      let suffix = interpolate && !properties.length ? '}' : ''
      let prefixAsValue = prefix + value + suffix + (properties.length > 0 ? '.' : '')

      if (root) {
        addSuggestion('', '', prefixAsValue)
      }

      if (interpolate) {
        addSuggestion('$', prefixAsValue.slice(1), prefixAsValue)
        addSuggestion('${', prefixAsValue.slice(2), prefixAsValue)
      }

      if (properties.length) {
        processProperties(prefix + value, properties, interpolate)
      }
    }
  })

  return result
}
