# [`pinctrl` module](<https://github.com/mariapatni/pinctrl>)

The pinctrl module implements a board component that accesses the hardware directly
to control GPIO pins. Pinctrl allows boards to configure pull up/pull down resistors
and switch between hardware PWM and GPIO modes.
Currently, the raspberry pi 5 is supported.


### Models
`viam-labs:pinctrl:rpi5`


### Attributes

#### pull
The pulls attribute is used to configure pull up and pull down resistors on pins.

The following attributes are avaliable for pulls:

| Attribute | Type | Required? | Description |
| --------- | ---- | --------- | ----------- |
| `pin` | string | **Required** | The physical pin number of the pin |
| `pull` | string | **Required** | The direction to pull  the pin. Options are "up", "down", "none". |


### Example Configuration
```json
  {
      "name": "pinctrl-pi5",
      "model": "viam-labs:pinctrl:rpi5",
      "type": "board",
      "namespace": "rdk",
      "attributes": {
        "pulls" : {
          [
          {
            "pin": "35",
            "pull": "up"
          },
            {
            "pin": "9",
            "pull": "down"
          },
          {
            "pin": "13",
            "pull": "none"
          },
          ]
        }
      }
  }
  ```

Note that this module is experimental.
