package loadpoint
import "testing"
func TestNextFusePhaseCapA(t *testing.T){
	const fuse,margin,step=16.0,1.0,1.0 // limit = 15
	cases:=[]struct{name string;prev,worst,want float64}{
		{"over-limit drops by overage",16,18,13},  // 16-(18-15)
		{"deadband holds",13,15,13},
		{"headroom ramps up",13,13,14},            // 13+1=14<=15
		{"never exceeds fuse",16,10,16},
		{"severe house overage floors at 0",2,20,0},
		{"uninit starts at fuse",0,10,16},          // 16, 10+1<=15 -> +1 -> 17 -> clamp 16
	}
	for _,tc:=range cases{
		if got:=nextFusePhaseCapA(tc.prev,tc.worst,fuse,margin,step);got!=tc.want{
			t.Errorf("%s: nextFusePhaseCapA(prev=%v,worst=%v)=%v want %v",tc.name,tc.prev,tc.worst,got,tc.want)
		}
	}
}
